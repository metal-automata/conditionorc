package store

/* This file implements the store interface backed by a NATS KV bucket.
   This bucket uses the server UUID as a key and uses the below-defined
   conditionRecord as its data.
*/

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/metrics"
	rctypes "github.com/metal-automata/rivets/condition"
	"github.com/metal-automata/rivets/events"
	"github.com/metal-automata/rivets/events/pkg/kv"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
)

var (
	ActiveConditionBucket = "active-conditions"
	bucketOpts            = []kv.Option{
		kv.WithDescription("tracking active conditions on servers"),
		kv.WithTTL(10 * 24 * time.Hour), // XXX: we could keep more history here, but might need more storage
	}
	errBadData = errors.New("bad condition data")
	ErrList    = errors.New("list query returned error")
)

type natsStore struct {
	log    *logrus.Logger
	bucket nats.KeyValue
}

func natsError(op string) {
	metrics.DependencyError("nats-active-conditions", op)
}

func newNatsRepository(log *logrus.Logger, stream events.Stream, replicaCount int) (*natsStore, error) {
	evJS, ok := stream.(*events.NatsJetstream)
	if !ok {
		// play stupid games, win stupid prizes
		panic("bad stream implementation")
	}

	kvOpts := bucketOpts
	if replicaCount > 1 {
		kvOpts = append(kvOpts, kv.WithReplicas(replicaCount))
	}

	kvHandle, err := kv.CreateOrBindKVBucket(evJS, ActiveConditionBucket, kvOpts...)
	if err != nil {
		log.WithError(err).Debug("binding kv bucket")
		return nil, errors.Wrap(err, "binding active conditions bucket")
	}
	return &natsStore{
		log:    log,
		bucket: kvHandle,
	}, nil
}

// List returns all conditions listed in the active-conditions KV.
func (n *natsStore) List(ctx context.Context) ([]*ConditionRecord, error) {
	_, span := otel.Tracer(pkgName).Start(ctx, "NatsStore.List")
	defer span.End()

	found := []*ConditionRecord{}
	watcher, err := n.bucket.WatchAll(nats.IgnoreDeletes())
	if err != nil {
		natsError("bucket.watchAll")
		return nil, errors.Wrap(err, "error listing all conditions")
	}

	defer func() {
		if errStop := watcher.Stop(); errStop != nil {
			n.log.WithError(errStop).Warn("error stopping watcher")
			natsError("watcher.stop")
		}
	}()

	for kve := range watcher.Updates() {
		if kve == nil {
			// this is weird, and it's also in their code. The channel isn't closed
			// until the internal watcher's subscription to the KV subject is shut down
			// so getting an explicit nil here means "nothing more."
			break
		}

		var cr ConditionRecord
		if err = cr.FromJSON(kve.Value()); err != nil {
			n.log.WithError(err).Warn("bad condition record")
			continue
		}

		found = append(found, &cr)
	}

	return found, nil
}

// Get returns the last ConditionRecord for activity on the server. This might be an active,
// or it might be history.
func (n *natsStore) Get(ctx context.Context, serverID uuid.UUID) (*ConditionRecord, error) {
	otelCtx, span := otel.Tracer(pkgName).Start(ctx, "NatsStore.Get")
	defer span.End()

	return n.getCurrentConditionRecord(otelCtx, serverID)
}

func (n *natsStore) getCurrentConditionRecord(ctx context.Context, serverID uuid.UUID) (*ConditionRecord, error) {
	_, span := otel.Tracer(pkgName).Start(ctx, "NatsStore.getCurrentConditionRecord")
	defer span.End()

	le := n.log.WithFields(logrus.Fields{
		"serverID": serverID.String(),
	})

	kve, err := n.bucket.Get(serverID.String())
	switch {
	case err == nil:
	case errors.Is(nats.ErrKeyNotFound, err):
		le.Debug("no active condition for device")
		return nil, ErrConditionNotFound
	default:
		natsError("get")
		le.WithError(err).Warn("looking up active condition")
		return nil, errors.Wrap(ErrRepository, err.Error())
	}

	// if we're here, we must have non-nil kve because NATS returns an error if a value isn't found for a key.
	var cr ConditionRecord
	if err = cr.FromJSON(kve.Value()); err != nil {
		le.WithError(err).Warn("bad condition data")
		return nil, errors.Wrap(ErrRepository, err.Error())
	}

	return &cr, nil
}

// GetActiveCondition returns any condition for the given server-id that is not in a final state.
// This is a locking mechanism to ensure that conditions (or sets of condtions) are only executed
// one-at-a-time. That is, even though "Pending" condition isn't "Active" per se, it is still
// just waiting on a controller and it's equivalent for the purposes of checking if another condition
// can be started.
func (n *natsStore) GetActiveCondition(ctx context.Context, serverID uuid.UUID) (*rctypes.Condition, error) {
	otelCtx, span := otel.Tracer(pkgName).Start(ctx, "NatsStore.GetActiveCondition")
	defer span.End()

	le := n.log.WithFields(logrus.Fields{
		"serverID": serverID.String(),
	})

	cr, err := n.getCurrentConditionRecord(otelCtx, serverID)
	if err != nil {
		return nil, err
	}

	if rctypes.StateIsComplete(cr.State) {
		return nil, ErrConditionNotFound
	}

	var active *rctypes.Condition
	for _, cond := range cr.Conditions {
		if !rctypes.StateIsComplete(cond.State) {
			le.WithFields(logrus.Fields{
				"condition.id":    cond.ID,
				"condition.state": string(cond.State),
			}).Trace("found active condition for server")
			active = cond
			break
		}
	}

	if active == nil {
		return nil, errors.Wrap(ErrConditionNotFound, "expected an active condition")
	}

	return active, nil
}

// Create one or more conditions on a server to define a unit of work to be done on a server.
// it checks for an existing, active ConditionRecord prior to queuing the new one, and will return an error if
// it finds one.
// @serverID: required
// @facilityCode: required
// @work: optional
func (n *natsStore) Create(ctx context.Context, serverID uuid.UUID, facilityCode string, work ...*rctypes.Condition) error {
	otelCtx, span := otel.Tracer(pkgName).Start(ctx, "NatsStore.Create")
	defer span.End()

	le := n.log.WithFields(logrus.Fields{
		"serverID": serverID.String(),
	})

	if len(work) == 0 {
		le.Info("no work to be done")
		return nil
	}

	cr, err := n.getCurrentConditionRecord(otelCtx, serverID)
	if err != nil && !errors.Is(err, ErrConditionNotFound) {
		return err
	}

	if cr != nil && !rctypes.StateIsComplete(cr.State) {
		activeID := cr.ID.String()
		le.WithField("active.condition.ID", activeID).Warn("existing active condition")
		return fmt.Errorf("%w:%s", ErrActiveCondition, cr.ID.String())
	}

	id := uuid.New()
	cr = &ConditionRecord{
		ID:       id,
		State:    rctypes.Pending,
		Facility: facilityCode,
	}
	for _, condition := range work {
		condition.ID = id
		condition.Target = serverID
		cr.Conditions = append(cr.Conditions, condition)
	}

	_, err = n.bucket.Put(serverID.String(), cr.MustJSON())
	if err != nil {
		natsError("create-multiple")
		span.RecordError(err)
		le.WithError(err).Warn("writing condition to storage")
	}
	return err
}

// Update a condition on a server.
// @id: required
// @condition: required
//
// Note: it's up to the caller to validate the condition update payload. The condition to update must
// exist, and it must be in a non-final state. The existing condition will be replaced by the incoming
// parameter. If applicable, the state of the ConditionRecord will be updated as well.
func (n *natsStore) Update(ctx context.Context, serverID uuid.UUID, updated *rctypes.Condition) error {
	otelCtx, span := otel.Tracer(pkgName).Start(ctx, "NatsStore.Update")
	defer span.End()

	le := n.log.WithFields(logrus.Fields{
		"serverID":      serverID.String(),
		"conditionKind": updated.Kind,
		"update.ID":     updated.ID.String(),
	})

	cr, err := n.getCurrentConditionRecord(otelCtx, serverID)
	if err != nil {
		span.RecordError(err)
		le.WithError(err).Warn("condition lookup failure on update")
		return err
	}

	// stupid games, stupid prizes sanity check
	if cr.ID != updated.ID {
		le.WithField("current.ID", cr.ID.String()).Warn("condition id mismatch")
		return errors.Wrap(ErrActiveCondition, "condition id mismatch")
	}

	// XXX: Having multiple conditions composed together into a single unit of work makes
	// managing the state of that unit a little more complicated than merely following the
	// state of the consitutent conditions. The state of the unit starts as Pending,
	// transitions to Active when the first update to Active for any condition arrives,
	// transitions to Failed if any condition fails, and transitions to Success when the
	// last condition is completed successfully. So to know whether a given success should
	// be applied to the unit, we need to know if this condition is the last.

	var lastCondition bool
	lastConditionPosition := len(cr.Conditions) - 1
	for idx, cond := range cr.Conditions {
		i := idx
		c := cond
		if c.Kind == updated.Kind {
			cr.Conditions[i] = updated
			if i == lastConditionPosition {
				lastCondition = true
			}
			break
		}
	}

	// XXX: is there a better way to OR 3 mostly independent conditions
	switch {
	case cr.State == rctypes.Pending, updated.State == rctypes.Failed, lastCondition:
		le.WithFields(
			logrus.Fields{
				"current.State": cr.State,
				"updated.State": updated.State,
				"lastCondition": lastCondition,
			},
		).Trace("Condition record state update")

		cr.State = updated.State
	default:
	}

	_, err = n.bucket.Put(serverID.String(), cr.MustJSON())
	if err != nil {
		natsError("update")
		span.RecordError(err)
		le.WithError(err).Warn("writing condition record on update")
		return err
	}

	return nil
}
