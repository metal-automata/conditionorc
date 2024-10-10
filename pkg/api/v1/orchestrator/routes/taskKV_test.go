package routes

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	rctypes "github.com/metal-automata/rivets/condition"
	"github.com/metal-automata/rivets/events"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nats-io/nats-server/v2/server"
	srvtest "github.com/nats-io/nats-server/v2/test"
)

func runNATSServer(t *testing.T) *server.Server {
	opts := srvtest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()

	return srvtest.RunServer(&opts)
}

func shutdownNATSServer(t *testing.T, s *server.Server) {
	t.Helper()
	s.Shutdown()
	s.WaitForShutdown()
}

func TestTaskKV(t *testing.T) {
	// Start a NATS server
	ns := runNATSServer(t)
	defer shutdownNATSServer(t, ns)

	nc, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer nc.Close()

	// nats JS from conn
	njs := events.NewJetstreamFromConn(nc)
	defer njs.Close()

	// create KV bucket
	js, err := nc.JetStream()
	require.NoError(t, err)
	_, err = js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket: rctypes.TaskKVRepositoryBucket,
	})
	require.NoError(t, err)

	// Create taskKVImpl
	logger := logrus.New()
	facilityCode := "area71"
	taskKV, err := initTaskKVImpl(facilityCode, logger, njs)
	require.NoError(t, err)

	ctx := context.Background()
	serverID := uuid.New()
	conditionID := uuid.New()
	conditionKind := rctypes.FirmwareInstall

	t.Run("Publish and Get Task", func(t *testing.T) {
		task := &rctypes.Task[any, any]{
			ID:    conditionID,
			Kind:  conditionKind,
			State: rctypes.Pending,
		}

		// Publish task
		err := taskKV.publish(ctx, serverID.String(), conditionID.String(), conditionKind, task, true)
		require.NoError(t, err)

		// Get task
		gotTask, err := taskKV.get(ctx, conditionKind, conditionID, serverID)
		require.NoError(t, err)

		assert.Equal(t, task.ID, gotTask.ID)
		assert.Equal(t, task.Kind, gotTask.Kind)
		assert.Equal(t, task.State, gotTask.State)
		assert.False(t, gotTask.CreatedAt.IsZero())
	})

	t.Run("Update Existing Task", func(t *testing.T) {
		updatedTask := &rctypes.Task[any, any]{
			ID:        conditionID,
			Kind:      conditionKind,
			State:     rctypes.Pending,
			UpdatedAt: time.Now(),
		}

		err := taskKV.publish(ctx, serverID.String(), conditionID.String(), conditionKind, updatedTask, false)
		require.NoError(t, err)

		retrievedTask, err := taskKV.get(ctx, conditionKind, conditionID, serverID)
		require.NoError(t, err)

		assert.Equal(t, updatedTask.State, retrievedTask.State)
		assert.True(t, retrievedTask.UpdatedAt.After(retrievedTask.CreatedAt))
	})

	t.Run("Update Timestamp Only", func(t *testing.T) {
		time.Sleep(time.Millisecond) // some buffer before we publish the ts update
		err := taskKV.publish(ctx, serverID.String(), conditionID.String(), conditionKind, nil, true)
		require.NoError(t, err)

		retrievedTask, err := taskKV.get(ctx, conditionKind, conditionID, serverID)
		require.NoError(t, err)

		assert.Equal(t, rctypes.Pending, retrievedTask.State)
		assert.True(t, retrievedTask.UpdatedAt.After(retrievedTask.CreatedAt))
	})

	t.Run("Update Existing Task - purge finalized", func(t *testing.T) {
		updatedTask := &rctypes.Task[any, any]{
			ID:        conditionID,
			Kind:      conditionKind,
			State:     rctypes.Succeeded,
			UpdatedAt: time.Now(),
		}

		err := taskKV.publish(ctx, serverID.String(), conditionID.String(), conditionKind, updatedTask, false)
		require.NoError(t, err)

		retrievedTask, err := taskKV.get(ctx, conditionKind, conditionID, serverID)
		require.NoError(t, err)

		assert.Equal(t, updatedTask.State, retrievedTask.State)
		assert.True(t, retrievedTask.UpdatedAt.After(retrievedTask.CreatedAt))
	})

	t.Run("Update Existing Task - purge stale", func(t *testing.T) {
		updatedTask := &rctypes.Task[any, any]{
			ID:        conditionID,
			Kind:      conditionKind,
			State:     rctypes.Succeeded,
			UpdatedAt: time.Now().Add(-rctypes.StaleThreshold + 1),
		}

		err := taskKV.publish(ctx, serverID.String(), conditionID.String(), conditionKind, updatedTask, false)
		require.NoError(t, err)

		retrievedTask, err := taskKV.get(ctx, conditionKind, conditionID, serverID)
		require.NoError(t, err)

		assert.Equal(t, updatedTask.State, retrievedTask.State)
		assert.False(t, retrievedTask.CreatedAt.IsZero())
	})

	t.Run("Get Non-existent Task", func(t *testing.T) {
		nonExistentID := uuid.New()
		_, err := taskKV.get(ctx, conditionKind, nonExistentID, nonExistentID)
		require.Error(t, err)
		assert.Contains(t, err.Error(), errNoTask.Error())
	})

	t.Run("Get Task returns stale entry error", func(t *testing.T) {
		// task object in KV is considered stale if the condition ID does not match the task ID
		_, err = taskKV.get(ctx, conditionKind, uuid.New(), serverID)
		assert.Contains(t, err.Error(), errStaleTask.Error())
	})
}
