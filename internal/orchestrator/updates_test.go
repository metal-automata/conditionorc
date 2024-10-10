// nolint
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/app"
	"github.com/metal-automata/conditionorc/internal/model"
	"github.com/metal-automata/conditionorc/internal/status"
	"github.com/metal-automata/conditionorc/internal/store"
	"github.com/metal-automata/rivets/events"
	"github.com/metal-automata/rivets/events/pkg/kv"
	"github.com/metal-automata/rivets/events/registry"
	"github.com/nats-io/nats-server/v2/server"
	srvtest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/conditions/types"
	rctypes "github.com/metal-automata/rivets/condition"
	eventsm "github.com/metal-automata/rivets/events"
)

var (
	nc         *nats.Conn
	js         nats.JetStreamContext
	evJS       *events.NatsJetstream
	logger     *logrus.Logger
	defs       rctypes.Definitions
	liveWorker = registry.GetID("updates-test")
)

func init() {
	logrus.SetFormatter(&logrus.JSONFormatter{})
}

func startJetStreamServer(t *testing.T) *server.Server {
	t.Helper()

	opts := srvtest.DefaultTestOptions
	opts.Port = -1
	opts.JetStream = true
	opts.StoreDir = t.TempDir()

	return srvtest.RunServer(&opts)
}

func jetStreamContext(s *server.Server) (*nats.Conn, nats.JetStreamContext) {
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		logger.Fatalf("connect => %v", err)
	}
	js, err := nc.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		logger.Fatalf("JetStream => %v", err)
	}
	return nc, js
}

func shutdownJetStream(t *testing.T, s *server.Server) {
	t.Helper()
	s.Shutdown()
	s.WaitForShutdown()
}

// let's pretend we're a conditioon-orchestrator app and do some one-time setup
func TestMain(m *testing.M) {
	logger = logrus.New()

	t := &testing.T{}
	srv := startJetStreamServer(t)
	defer shutdownJetStream(t, srv)
	nc, js = jetStreamContext(srv) // nc is closed on evJS.Close(), js needs no cleanup
	evJS = events.NewJetstreamFromConn(nc)
	defer evJS.Close()

	defs = rctypes.Definitions{
		&rctypes.Definition{
			Kind: rctypes.FirmwareInstall,
		},
		&rctypes.Definition{
			Kind: rctypes.Inventory,
		},
		&rctypes.Definition{
			Kind: rctypes.Kind("bogus"),
		},
	}

	status.ConnectToKVStores(evJS, logger, defs, kv.WithDescription("test watchers"))
	registry.InitializeRegistryWithOptions(evJS) // we don't need a TTL or replicas
	if err := registry.RegisterController(liveWorker); err != nil {
		logger.WithError(err).Fatal("registering controller id")
	}

	exitCode := m.Run()
	os.Exit(exitCode)
}

func TestParseStatusKey(t *testing.T) {
	t.Parallel()
	goodKey := "fc13.0099138a-2645-4c27-afe6-a30b613f59ae"
	periods := "too.many.periods"
	badId := "fc13.not-a-uuuid"

	key, err := parseStatusKVKey(goodKey)
	require.NoError(t, err)
	require.Equal(t, uuid.MustParse("0099138a-2645-4c27-afe6-a30b613f59ae"), key.conditionID)

	_, err = parseStatusKVKey(periods)
	require.Error(t, err)
	require.ErrorIs(t, err, errKeyFormat)

	_, err = parseStatusKVKey(badId)
	require.Error(t, err)
	require.ErrorIs(t, err, errConditionID)
}

func newCleanStatusKV(t *testing.T, kind rctypes.Kind) nats.KeyValue {
	t.Helper()

	sKV, err := status.GetConditionKV(kind)
	if err != nil {
		t.Fatal(err)
	}

	kvDeleteAll(t, sKV)
	return sKV
}

func newCleanActiveConditionsKV(t *testing.T) nats.KeyValue {
	t.Helper()

	// active-conditions KV handle
	acKV, err := kv.CreateOrBindKVBucket(evJS, store.ActiveConditionBucket)
	if err != nil {
		t.Fatal(err)
	}

	kvDeleteAll(t, acKV)
	return acKV
}

func kvDeleteAll(t *testing.T, kv nats.KeyValue) {
	t.Helper()

	// clean up all keys for this test
	purge, err := kv.Keys()
	if err != nil && !errors.Is(err, nats.ErrNoKeysFound) {
		t.Fatal(err)
	}

	for _, k := range purge {
		if err := kv.Delete(k); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
			t.Fatal(err)
		}
	}
}

func TestActiveConditionsToReconcile_Creates(t *testing.T) {
	o := Orchestrator{
		logger:        logger,
		streamBroker:  evJS,
		facility:      "fc13",
		conditionDefs: defs,
	}

	cfg := &app.Configuration{StoreKind: model.NATS}
	repository, err := store.NewStore(cfg, logger, evJS)
	if err != nil {
		t.Fatal(err)
	}
	o.repository = repository

	type testConditionSV struct {
		conditionID  uuid.UUID
		statusValue  *rctypes.StatusValue
		facilityCode string
	}

	// conditions to be created
	sid := uuid.New()
	cid := uuid.New()

	testcases := []struct {
		name string
		// condition statusValues to be written to the status KV
		conditionStatusValues []*testConditionSV
		// conditions to be written as condition records to the active-conditions KV
		conditions              []*rctypes.Condition
		wantCreates             int
		wantUpdates             int
		cleanActiveConditionsKV bool
		cleanStatusKV           bool
	}{
		{
			// two statusValue KV records created, with no record in active-conditions KV
			// expect two create objects returned
			name: "test creates",
			conditionStatusValues: []*testConditionSV{
				{
					conditionID: uuid.New(),
					statusValue: &rctypes.StatusValue{
						Target:    uuid.New().String(),
						State:     string(rctypes.Active),
						Status:    json.RawMessage(fmt.Sprintf(`{"msg":"foo", "facility": "%s"}`, o.facility)),
						WorkerID:  registry.GetID("test1").String(),
						CreatedAt: time.Now().Add(-20 * time.Minute),
					},
					facilityCode: o.facility,
				},
				{
					conditionID: uuid.New(),
					statusValue: &rctypes.StatusValue{
						Target:    uuid.New().String(),
						State:     string(rctypes.Pending),
						Status:    json.RawMessage(fmt.Sprintf(`{"msg":"foo", "facility": "%s"}`, o.facility)),
						WorkerID:  registry.GetID("test2").String(),
						CreatedAt: time.Now().Add(-20 * time.Minute),
					},
					facilityCode: o.facility,
				},
			},
			wantCreates:             2,
			wantUpdates:             0,
			cleanActiveConditionsKV: true,
			cleanStatusKV:           true,
		},
		{
			// two statusValue KV records created, with no record in active-conditions KV,
			// one statusValue is created in a different facility KV,
			// expect one create object returned
			name: "test creates - only on assigned facility",
			conditionStatusValues: []*testConditionSV{
				{
					conditionID: uuid.New(),
					statusValue: &rctypes.StatusValue{
						Target:    uuid.New().String(),
						State:     string(rctypes.Active),
						Status:    json.RawMessage(fmt.Sprintf(`{"msg":"foo", "facility": "%s"}`, o.facility)),
						WorkerID:  registry.GetID("test1").String(),
						CreatedAt: time.Now().Add(-20 * time.Minute),
					},
					facilityCode: o.facility,
				},
				{
					conditionID: uuid.New(),
					statusValue: &rctypes.StatusValue{
						Target:    uuid.New().String(),
						State:     string(rctypes.Pending),
						Status:    json.RawMessage(fmt.Sprintf(`{"msg":"foo", "facility": "%s"}`, "rando13")),
						WorkerID:  registry.GetID("test2").String(),
						CreatedAt: time.Now().Add(-20 * time.Minute),
					},
					facilityCode: "rando13",
				},
			},
			wantCreates:             1,
			wantUpdates:             0,
			cleanActiveConditionsKV: true,
			cleanStatusKV:           true,
		},
		{
			name: "test no creates",
			conditionStatusValues: []*testConditionSV{
				{
					conditionID: cid,
					statusValue: &rctypes.StatusValue{
						Target:    sid.String(),
						State:     string(rctypes.Pending),
						Status:    json.RawMessage(fmt.Sprintf(`{"msg":"foo", "facility": "%s"}`, o.facility)),
						WorkerID:  registry.GetID("test2").String(),
						CreatedAt: time.Now().Add(-20 * time.Minute),
					},
					facilityCode: o.facility,
				},
			},
			conditions: []*rctypes.Condition{
				{
					ID:        cid,
					Kind:      rctypes.FirmwareInstall,
					State:     rctypes.Pending,
					Target:    sid,
					CreatedAt: time.Now().Add(-20 * time.Minute),
				},
			},
			wantCreates:             0,
			wantUpdates:             0,
			cleanActiveConditionsKV: true,
			cleanStatusKV:           true,
		},
	}

	fwsKV := newCleanStatusKV(t, rctypes.FirmwareInstall)
	_ = newCleanActiveConditionsKV(t)

	for idx, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// clean when its not the first testcase and the testcase requires a clean.
			if idx != 0 && tc.cleanStatusKV {
				fwsKV = newCleanStatusKV(t, rctypes.FirmwareInstall)
			}

			// clean when its not the first testcase and the testcase requires a clean.
			if idx != 0 && tc.cleanActiveConditionsKV {
				_ = newCleanActiveConditionsKV(t)
			}

			for _, condSV := range tc.conditionStatusValues {
				key := fmt.Sprintf("%s.%s", condSV.facilityCode, condSV.conditionID)
				_, err = fwsKV.Put(key, condSV.statusValue.MustBytes())
				require.NoError(t, err)
			}

			ctx := context.Background()
			for _, cond := range tc.conditions {
				err := o.repository.Create(ctx, cond.Target, o.facility, cond)
				require.NoError(t, err)
			}

			gotCreates, gotUpdates := o.activeConditionsToReconcile(ctx)
			assert.Len(t, gotCreates, tc.wantCreates, "creates match")
			assert.Len(t, gotUpdates, tc.wantUpdates, "updates match")

			// verify the method only reconciles conditions for the facility this orchestrator configured for.
			for _, create := range gotCreates {
				assert.Contains(t, string(create.Status), o.facility)
			}
		})
	}
}

func TestActiveConditionsToReconcile_Updates(t *testing.T) {
	o := Orchestrator{
		logger:       logger,
		streamBroker: evJS,
		facility:     "fc-13",
	}

	cfg := &app.Configuration{StoreKind: model.NATS}
	repository, err := store.NewStore(cfg, logger, evJS)
	if err != nil {
		t.Fatal(err)
	}
	o.repository = repository

	ctx := context.Background()

	// conditions to be created
	sid := uuid.New()
	cid := uuid.New()
	fwcond := &rctypes.Condition{
		ID:        cid,
		Kind:      rctypes.FirmwareInstall,
		State:     rctypes.Pending,
		Target:    sid,
		CreatedAt: time.Now(),
	}

	invcond := &rctypes.Condition{
		ID:        cid,
		Kind:      rctypes.Inventory,
		State:     rctypes.Pending,
		Target:    sid,
		CreatedAt: time.Now(),
	}

	// active-conditions handle
	acKV := newCleanActiveConditionsKV(t)

	if err := o.repository.Create(ctx, sid, o.facility, fwcond, invcond); err != nil {
		t.Fatal(err)
	}

	records, err := o.repository.List(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// make sure we have one record added
	assert.Len(t, records, 1)

	// record in pending state
	creates, updates := o.activeConditionsToReconcile(ctx)
	assert.Len(t, creates, 0, "creates")
	assert.Len(t, updates, 0, "updates")

	// get current active-condition entry
	entry, err := acKV.Get(sid.String())
	if err != nil {
		t.Fatal(err)
	}

	var cr store.ConditionRecord
	if err := cr.FromJSON(entry.Value()); err != nil {
		t.Fatal(err)
	}

	// set condition CreatedAt to across the stale threshold - condition in queue, not exceeded queue age threshold
	cr.Conditions[0].CreatedAt = time.Now().Add(-1 * rctypes.StaleThreshold)

	o.repository.Update(ctx, sid, cr.Conditions[0])
	if err != nil {
		t.Fatal(err)
	}

	// expect updates and no creates
	creates, updates = o.activeConditionsToReconcile(ctx)
	assert.Len(t, creates, 0, "creates")
	assert.Len(t, updates, 0, "updates")

	// set condition CreatedAt to across the stale threshold - condition in queue, exceeded queue age threshold
	cr.Conditions[0].CreatedAt = time.Now().Add(-1 * msgMaxAgeThreshold)
	cr.Conditions[0].UpdatedAt = time.Time{}

	o.repository.Update(ctx, sid, cr.Conditions[0])
	if err != nil {
		t.Fatal(err)
	}

	// expect updates and no creates
	creates, updates = o.activeConditionsToReconcile(ctx)
	assert.Len(t, creates, 0, "creates")
	assert.Len(t, updates, 1, "updates")
}

func TestEventNeedsReconciliation(t *testing.T) {
	t.Parallel()

	o := Orchestrator{
		logger: logger,
	}

	evt := &v1types.ConditionUpdateEvent{
		ConditionUpdate: v1types.ConditionUpdate{
			UpdatedAt: time.Now(),
		},
		ControllerID: registry.GetID("needs-reconciliation-test"),
	}
	require.False(t, o.eventNeedsReconciliation(evt), "Condition UpdatedAt")

	evt.UpdatedAt = time.Now().Add(-1 * rctypes.StaleThreshold)
	evt.ConditionUpdate.State = rctypes.Failed
	require.True(t, o.eventNeedsReconciliation(evt), "Condition finalized")

	evt.ConditionUpdate.State = rctypes.Active
	require.True(t, o.eventNeedsReconciliation(evt), "Condition finalized")

	evt.ControllerID = liveWorker
	require.False(t, o.eventNeedsReconciliation(evt), "controller active")
}

func TestFilterToReconcile(t *testing.T) {
	t.Parallel()

	// test server, condition ID 1
	cid1 := uuid.New()
	sid1 := uuid.New()

	// test server, condition ID 2
	cid2 := uuid.New()
	sid2 := uuid.New()

	// test timestamps
	createdTS := time.Now()
	updatedTS := createdTS.Add(1 * time.Minute)

	facility := "fac22"

	tests := []struct {
		name         string
		records      []*store.ConditionRecord                 // records in active-conditions
		updateEvents map[string]*v1types.ConditionUpdateEvent // status updates from status KV
		wantCreates  []*rctypes.Condition                     // expected creates
		wantUpdates  []*v1types.ConditionUpdateEvent          // expected updates
	}{
		{
			name: "pending in active-conditions and pending in status KV",
			records: []*store.ConditionRecord{
				{
					ID:       cid1,
					State:    rctypes.Pending,
					Facility: facility,
					Conditions: []*rctypes.Condition{
						{
							ID:     cid1,
							Kind:   rctypes.FirmwareInstall,
							State:  rctypes.Pending,
							Target: sid1,
						},
						{
							ID:     cid1,
							Kind:   rctypes.Inventory,
							State:  rctypes.Pending,
							Target: sid1,
						},
					},
				},
			},
			updateEvents: map[string]*v1types.ConditionUpdateEvent{
				sid1.String(): {
					ConditionUpdate: v1types.ConditionUpdate{
						ServerID: sid1,
						State:    rctypes.Pending,
					},
				},
			},
			wantCreates: nil,
			wantUpdates: nil,
		},
		{
			name: "pending in active-conditions exceeded stale threshold and not listed in status KV",
			records: []*store.ConditionRecord{
				{
					ID:       cid1,
					State:    rctypes.Pending,
					Facility: facility,
					Conditions: []*rctypes.Condition{
						{
							ID:     cid1,
							Kind:   rctypes.FirmwareInstall,
							State:  rctypes.Pending,
							Target: sid1,
							// exceed thresholds
							CreatedAt: createdTS.Add(-rctypes.StaleThreshold - 2*time.Minute),
							UpdatedAt: updatedTS.Add(-rctypes.StatusStaleThreshold - 2*time.Minute),
						},
						{
							ID:        cid1,
							Kind:      rctypes.Inventory,
							State:     rctypes.Pending,
							Target:    sid1,
							CreatedAt: createdTS.Add(-rctypes.StaleThreshold + 1),
							UpdatedAt: time.Time{},
						},
					},
				},
			},
			updateEvents: map[string]*v1types.ConditionUpdateEvent{},
			wantCreates:  nil,
			wantUpdates: []*v1types.ConditionUpdateEvent{
				{
					ConditionUpdate: v1types.ConditionUpdate{
						ConditionID: cid1,
						ServerID:    sid1,
						State:       rctypes.Failed,
						Status:      failedByReconciler,
						CreatedAt:   createdTS.Add(-rctypes.StaleThreshold - 2*time.Minute),
					},
					Kind: rctypes.FirmwareInstall,
				},
			},
		},
		{
			name: "CR with no conditions",
			records: []*store.ConditionRecord{
				{
					ID:         cid1,
					State:      rctypes.Pending,
					Facility:   facility,
					Conditions: []*rctypes.Condition{},
				},
			},
			updateEvents: map[string]*v1types.ConditionUpdateEvent{},
			wantCreates:  nil,
			wantUpdates:  nil,
		},
		{
			name: "facility not matched",
			records: []*store.ConditionRecord{
				{
					ID:       cid1,
					State:    rctypes.Pending,
					Facility: "mismatch",
					Conditions: []*rctypes.Condition{
						{
							ID:     cid1,
							Kind:   rctypes.FirmwareInstall,
							State:  rctypes.Pending,
							Target: sid1,
							// exceed thresholds
							CreatedAt: createdTS,
							UpdatedAt: updatedTS,
						},
						{
							ID:        cid1,
							Kind:      rctypes.Inventory,
							State:     rctypes.Pending,
							Target:    sid1,
							CreatedAt: createdTS.Add(-rctypes.StaleThreshold + 1),
							UpdatedAt: time.Time{},
						},
					},
				},
			},
			updateEvents: map[string]*v1types.ConditionUpdateEvent{},
			wantCreates:  nil,
			wantUpdates:  nil,
		},
		{
			name: "CR is in complete state",
			records: []*store.ConditionRecord{
				{
					ID:       cid1,
					State:    rctypes.Succeeded,
					Facility: facility,
					Conditions: []*rctypes.Condition{
						{
							ID:     cid1,
							Kind:   rctypes.FirmwareInstall,
							State:  rctypes.Pending,
							Target: sid1,
							// exceed thresholds
							CreatedAt: createdTS,
							UpdatedAt: updatedTS,
						},
						{
							ID:        cid1,
							Kind:      rctypes.Inventory,
							State:     rctypes.Pending,
							Target:    sid1,
							CreatedAt: createdTS.Add(-rctypes.StaleThreshold + 1),
							UpdatedAt: time.Time{},
						},
					},
				},
			},
			updateEvents: map[string]*v1types.ConditionUpdateEvent{},
			wantCreates:  nil,
			wantUpdates:  nil,
		},
		{
			name: "CR in complete state, status KV record in complete state",
			records: []*store.ConditionRecord{
				{
					ID:       cid1,
					State:    rctypes.Succeeded,
					Facility: facility,
					Conditions: []*rctypes.Condition{
						{
							ID:        cid1,
							Kind:      rctypes.FirmwareInstall,
							State:     rctypes.Succeeded,
							Target:    sid1,
							CreatedAt: createdTS,
							UpdatedAt: updatedTS,
						},
						{
							ID:        cid1,
							Kind:      rctypes.Inventory,
							State:     rctypes.Succeeded,
							Target:    sid1,
							CreatedAt: createdTS,
							UpdatedAt: updatedTS,
						},
					},
				},
			},
			updateEvents: map[string]*v1types.ConditionUpdateEvent{
				sid1.String(): {
					Kind: rctypes.Inventory,
					ConditionUpdate: v1types.ConditionUpdate{
						ConditionID: cid1,
						ServerID:    sid1,
						State:       rctypes.Succeeded,
						Status:      []byte(`{"msg": "all done"}`),
						UpdatedAt:   updatedTS,
						CreatedAt:   createdTS,
					},
				},
			},
			wantCreates: nil,
			wantUpdates: nil,
		},
		{
			name: "CR in Pending state and Condition is still in queue",
			records: []*store.ConditionRecord{
				{
					ID:       cid1,
					State:    rctypes.Pending,
					Facility: facility,
					Conditions: []*rctypes.Condition{
						{
							ID:        cid1,
							Kind:      rctypes.FirmwareInstall,
							State:     rctypes.Pending,
							Target:    sid1,
							CreatedAt: createdTS.Add(-rctypes.StaleThreshold - 2*time.Minute),
							UpdatedAt: time.Time{},
						},
						{
							ID:        cid1,
							Kind:      rctypes.Inventory,
							State:     rctypes.Pending,
							Target:    sid1,
							CreatedAt: createdTS.Add(-rctypes.StaleThreshold + 1),
							UpdatedAt: time.Time{},
						},
					},
				},
			},
			updateEvents: map[string]*v1types.ConditionUpdateEvent{},
			wantCreates:  nil,
			wantUpdates:  nil,
		},
		{
			name: "CR in Pending state within stale threshold and not listed in status KV",
			records: []*store.ConditionRecord{
				{
					ID:       cid1,
					State:    rctypes.Pending,
					Facility: facility,
					Conditions: []*rctypes.Condition{
						{
							ID:        cid1,
							Kind:      rctypes.FirmwareInstall,
							State:     rctypes.Pending,
							Target:    sid1,
							CreatedAt: createdTS,
							UpdatedAt: updatedTS,
						},
						{
							ID:        cid1,
							Kind:      rctypes.Inventory,
							State:     rctypes.Pending,
							Target:    sid1,
							CreatedAt: createdTS,
							UpdatedAt: updatedTS,
						},
					},
				},
			},
			updateEvents: map[string]*v1types.ConditionUpdateEvent{},
			wantCreates:  nil,
			wantUpdates:  nil,
		},
		{
			name:    "not listed in active-conditions and in-complete in status KV",
			records: []*store.ConditionRecord{},
			updateEvents: map[string]*v1types.ConditionUpdateEvent{
				sid1.String(): {
					ConditionUpdate: v1types.ConditionUpdate{
						ConditionID: cid1,
						ServerID:    sid1,
						State:       rctypes.Active,
						Status:      []byte(`{"msg": "running"}`),
						UpdatedAt:   updatedTS,
						CreatedAt:   createdTS,
					},
				},
			},
			wantCreates: []*rctypes.Condition{
				{
					Version:   rctypes.ConditionStructVersion,
					ID:        cid1,
					Target:    sid1,
					State:     rctypes.Active,
					Status:    []byte(`{"msg": "running"}`),
					UpdatedAt: updatedTS,
					CreatedAt: createdTS,
				},
			},
			wantUpdates: nil,
		},
		{
			name: "multiple updates and creates",
			records: []*store.ConditionRecord{
				// record to be updated
				{
					ID:       cid1,
					State:    rctypes.Active,
					Facility: facility,
					Conditions: []*rctypes.Condition{
						{
							ID:     cid1,
							Kind:   rctypes.FirmwareInstall,
							State:  rctypes.Pending,
							Target: sid1,
							// thresholds on record exceeded
							CreatedAt: createdTS.Add(-rctypes.StaleThreshold - 2*time.Minute),
							UpdatedAt: updatedTS.Add(-rctypes.StatusStaleThreshold - 2*time.Minute),
						},
					},
				},
			},
			updateEvents: map[string]*v1types.ConditionUpdateEvent{
				// expect create for event
				sid2.String(): {
					ConditionUpdate: v1types.ConditionUpdate{
						ConditionID: cid2,
						ServerID:    sid2,
						State:       rctypes.Active,
						Status:      []byte(`{"msg": "running"}`),
						UpdatedAt:   updatedTS,
						CreatedAt:   createdTS,
					},
				},
			},
			wantCreates: []*rctypes.Condition{
				{
					Version:   rctypes.ConditionStructVersion,
					ID:        cid2,
					Target:    sid2,
					State:     rctypes.Active,
					Status:    []byte(`{"msg": "running"}`),
					UpdatedAt: updatedTS,
					CreatedAt: createdTS,
				},
			},
			wantUpdates: []*v1types.ConditionUpdateEvent{
				{
					ConditionUpdate: v1types.ConditionUpdate{
						ConditionID: cid1,
						ServerID:    sid1,
						State:       rctypes.Failed,
						Status:      failedByReconciler,
						CreatedAt:   createdTS.Add(-rctypes.StaleThreshold - 2*time.Minute),
					},
					Kind: rctypes.FirmwareInstall,
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotCreates, gotUpdates := filterToReconcile(tc.records, tc.updateEvents, facility)
			if len(tc.wantCreates) == 0 {
				assert.Len(t, gotCreates, 0)
			} else {
				assert.Equal(t, tc.wantCreates, gotCreates)
			}

			if len(tc.wantUpdates) == 0 {
				assert.Len(t, gotUpdates, 0)
			} else {
				assert.Len(t, gotUpdates, 1)
				assert.WithinDuration(t, time.Now(), gotUpdates[0].UpdatedAt, 3*time.Second)
				gotUpdates[0].UpdatedAt = time.Time{}
				assert.Equal(t, gotUpdates, tc.wantUpdates)
			}
		})
	}
}

func TestEventUpdateFromKV(t *testing.T) {
	writeHandle, err := status.GetConditionKV(rctypes.FirmwareInstall)
	require.NoError(t, err, "write handle")

	cID := registry.GetID("test-app")

	// add some KVs
	sv1 := rctypes.StatusValue{
		Target:   uuid.New().String(),
		State:    "pending",
		Status:   json.RawMessage(`{"msg":"some-status"}`),
		WorkerID: cID.String(),
	}
	bogus := rctypes.StatusValue{
		Target: uuid.New().String(),
		State:  "bogus",
		Status: json.RawMessage(`{"msg":"some-status"}`),
	}
	noCID := rctypes.StatusValue{
		Target:    uuid.New().String(),
		State:     "failed",
		Status:    json.RawMessage(`{"msg":"some-status"}`),
		UpdatedAt: time.Now().Add(-90 * time.Minute),
	}

	condID := uuid.New()
	k1 := fmt.Sprintf("fc13.%s", condID)
	k2 := fmt.Sprintf("fc13.%s", uuid.New())
	k3 := fmt.Sprintf("fc13.%s", uuid.New())

	_, err = writeHandle.Put(k1, sv1.MustBytes())
	require.NoError(t, err)

	_, err = writeHandle.Put(k2, bogus.MustBytes())
	require.NoError(t, err)

	_, err = writeHandle.Put(k3, noCID.MustBytes())
	require.NoError(t, err)

	// test the expected good KV entry
	entry, err := writeHandle.Get(k1)
	require.NoError(t, err)

	upd1, err := parseEventUpdateFromKV(context.Background(), entry, rctypes.FirmwareInstall)
	require.NoError(t, err)
	require.Equal(t, condID, upd1.ConditionUpdate.ConditionID)
	require.Equal(t, rctypes.Pending, upd1.ConditionUpdate.State)

	// bogus state should error
	entry, err = writeHandle.Get(k2)
	require.NoError(t, err)
	_, err = parseEventUpdateFromKV(context.Background(), entry, rctypes.FirmwareInstall)
	require.ErrorIs(t, errInvalidState, err)

	// no controller id event should error as well
	entry, err = writeHandle.Get(k3)
	require.NoError(t, err)
	_, err = parseEventUpdateFromKV(context.Background(), entry, rctypes.FirmwareInstall)
	require.ErrorIs(t, err, registry.ErrBadFormat)
}

func TestConditionListenersExit(t *testing.T) {
	ic := goleak.IgnoreCurrent()
	defer goleak.VerifyNone(t, ic)

	o := Orchestrator{
		logger:        logger,
		streamBroker:  evJS,
		facility:      "test",
		conditionDefs: defs,
	}

	// here we're acting in place of kvStatusPublisher to make sure that we can orchestrate the watchers
	testChan := make(chan *v1types.ConditionUpdateEvent)
	ctx, cancel := context.WithCancel(context.TODO())

	wg := &sync.WaitGroup{}
	o.startConditionWatchers(ctx, testChan, wg)
	o.startReconciler(ctx, wg)

	sentinelChan := make(chan struct{})
	toCtx, toCancel := context.WithTimeout(context.TODO(), time.Second)
	defer toCancel()

	var testPassed bool

	go func() {
		wg.Wait()
		testPassed = true
		close(sentinelChan)
	}()

	// cancel the original context and we should unwind all our listeners
	cancel()

	select {
	case <-toCtx.Done():
		t.Log("watchers did not exit")
	case <-sentinelChan:
	}
	require.True(t, testPassed)
}

func TestQueueFollowingCondition(t *testing.T) {
	serverID := uuid.New()
	t.Run("no following work", func(t *testing.T) {
		condID := uuid.New()
		repo := store.NewMockRepository(t)
		repo.On("GetActiveCondition", mock.Anything, serverID).Return(nil, store.ErrConditionNotFound).Once()
		o := &Orchestrator{
			logger:     logger,
			repository: repo,
		}
		condArg := &rctypes.Condition{ID: condID, Target: serverID}
		require.NoError(t, o.queueFollowingCondition(context.TODO(), condArg))
	})
	t.Run("lookup error", func(t *testing.T) {
		condID := uuid.New()
		repo := store.NewMockRepository(t)
		repo.On("GetActiveCondition", mock.Anything, serverID).Return(nil, errors.New("pound sand")).Once()
		o := &Orchestrator{
			logger:     logger,
			repository: repo,
		}
		condArg := &rctypes.Condition{ID: condID, Target: serverID}
		err := o.queueFollowingCondition(context.TODO(), condArg)
		require.Error(t, err)
		require.ErrorIs(t, err, errCompleteEvent)
	})
	t.Run("publishing error", func(t *testing.T) {
		condID := uuid.New()
		repo := store.NewMockRepository(t)
		next := &rctypes.Condition{
			ID:     condID,
			Kind:   rctypes.Kind("following-kind"),
			Target: serverID,
			State:  rctypes.Pending,
		}
		condArg := &rctypes.Condition{ID: condID, Target: serverID}
		repo.On("GetActiveCondition", mock.Anything, serverID).Return(next, nil).Once()
		stream := eventsm.NewMockStream(t)
		subject := "fc-13.servers.following-kind"
		stream.On("Publish", mock.Anything, subject, mock.Anything).Return(errors.New("pound sand")).Once()
		o := &Orchestrator{
			facility:     "fc-13",
			logger:       logger,
			repository:   repo,
			streamBroker: stream,
		}
		err := o.queueFollowingCondition(context.TODO(), condArg)
		require.Error(t, err)
		require.ErrorIs(t, err, errCompleteEvent)
	})
	t.Run("next condition state is not pending", func(t *testing.T) {
		condID := uuid.New()
		repo := store.NewMockRepository(t)
		next := &rctypes.Condition{
			ID:     condID,
			Kind:   rctypes.Kind("following-kind"),
			Target: serverID,
			State:  rctypes.Active,
		}
		condArg := &rctypes.Condition{ID: condID, Target: serverID}
		repo.On("GetActiveCondition", mock.Anything, serverID).Return(next, nil).Once()
		o := &Orchestrator{
			logger:     logger,
			repository: repo,
		}
		err := o.queueFollowingCondition(context.TODO(), condArg)
		require.NoError(t, err)
	})
	t.Run("successful publish", func(t *testing.T) {
		condID := uuid.New()
		repo := store.NewMockRepository(t)
		next := &rctypes.Condition{
			ID:     condID,
			Kind:   rctypes.Kind("following-kind"),
			Target: serverID,
			State:  rctypes.Pending,
		}
		condArg := &rctypes.Condition{ID: condID, Target: serverID}
		repo.On("GetActiveCondition", mock.Anything, serverID).Return(next, nil).Once()
		stream := eventsm.NewMockStream(t)
		subject := "fc-13.servers.following-kind"
		stream.On("Publish", mock.Anything, subject, mock.Anything).Return(nil).Once()
		o := &Orchestrator{
			facility:     "fc-13",
			logger:       logger,
			repository:   repo,
			streamBroker: stream,
		}
		err := o.queueFollowingCondition(context.TODO(), condArg)
		require.NoError(t, err)
	})
}
