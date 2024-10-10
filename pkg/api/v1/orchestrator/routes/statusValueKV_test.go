package routes

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/status"
	"github.com/metal-automata/rivets/events"
	"github.com/metal-automata/rivets/events/registry"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rctypes "github.com/metal-automata/rivets/condition"
)

func TestStatusValuePublish(t *testing.T) {
	// Start a NATS server
	ns := runNATSServer(t)
	defer shutdownNATSServer(t, ns)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	// Nats conn as nats JS
	njs := events.NewJetstreamFromConn(nc)
	defer njs.Close()

	// Initialize status KV stores
	defs := rctypes.Definitions{
		{
			Kind: rctypes.FirmwareInstall,
		},
	}

	status.ConnectToKVStores(njs, &logrus.Logger{}, defs)

	// init statusValue
	sv := initStatusValueKV()

	facilityCode := "test-facility"
	conditionID := uuid.New()
	conditionKind := rctypes.FirmwareInstall
	serverID := uuid.New()
	controllerID := registry.GetIDWithUUID("test", serverID)

	t.Run("Create new status value", func(t *testing.T) {
		newSV := &rctypes.StatusValue{
			WorkerID: controllerID.String(),
			Target:   serverID.String(),
			State:    string(rctypes.Pending),
			Status:   json.RawMessage(`{"message":"woot"}`),
		}

		err := sv.publish(facilityCode, conditionID, conditionKind, newSV, true)
		require.NoError(t, err)

		// Verify the status value was created
		statusKV, err := status.GetConditionKV(conditionKind)
		require.NoError(t, err)

		key := rctypes.StatusValueKVKey(facilityCode, conditionID.String())
		entry, err := statusKV.Get(key)
		require.NoError(t, err)

		var retrievedSV rctypes.StatusValue
		err = json.Unmarshal(entry.Value(), &retrievedSV)
		require.NoError(t, err)

		assert.Equal(t, newSV.WorkerID, retrievedSV.WorkerID)
		assert.Equal(t, newSV.State, retrievedSV.State)
		assert.Equal(t, newSV.Status, retrievedSV.Status)
		assert.False(t, retrievedSV.CreatedAt.IsZero())
	})

	t.Run("Update existing status value", func(t *testing.T) {
		updatedSV := &rctypes.StatusValue{
			WorkerID: controllerID.String(),
			Target:   serverID.String(),
			State:    string(rctypes.Active),
			Status:   json.RawMessage(`{"message":"woot woot"}`),
		}

		err := sv.publish(facilityCode, conditionID, conditionKind, updatedSV, false)
		require.NoError(t, err)

		// Verify the status value was updated
		statusKV, err := status.GetConditionKV(conditionKind)
		require.NoError(t, err)

		key := rctypes.StatusValueKVKey(facilityCode, conditionID.String())
		entry, err := statusKV.Get(key)
		require.NoError(t, err)

		var retrievedSV rctypes.StatusValue
		err = json.Unmarshal(entry.Value(), &retrievedSV)
		require.NoError(t, err)

		assert.Equal(t, updatedSV.WorkerID, retrievedSV.WorkerID)
		assert.Equal(t, updatedSV.State, retrievedSV.State)
		assert.Equal(t, updatedSV.Status, retrievedSV.Status)
		assert.False(t, retrievedSV.UpdatedAt.IsZero())
	})

	t.Run("Update timestamp only", func(t *testing.T) {
		err := sv.publish(facilityCode, conditionID, conditionKind, nil, true)
		require.NoError(t, err)

		// Verify only the updatedAt timestamp was updated
		statusKV, err := status.GetConditionKV(conditionKind)
		key := rctypes.StatusValueKVKey(facilityCode, conditionID.String())
		entry, err := statusKV.Get(key)
		require.NoError(t, err)

		var retrievedSV rctypes.StatusValue
		err = json.Unmarshal(entry.Value(), &retrievedSV)
		require.NoError(t, err)

		assert.Equal(t, string(rctypes.Active), retrievedSV.State) // Should remain unchanged
		assert.True(t, retrievedSV.UpdatedAt.After(retrievedSV.CreatedAt))
	})
}
