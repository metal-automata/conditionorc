package routes

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	rctypes "github.com/metal-automata/rivets/condition"
	"github.com/metal-automata/rivets/events"
	"github.com/metal-automata/rivets/events/pkg/kv"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var (
	errPublishTask = errors.New("error in condition task publish")
	errNoTask      = errors.New("no task available")
	errQueryTask   = errors.New("error in condition task query")
	errStaleTask   = errors.New("stale task in kv")
)

type taskKV interface {
	get(
		ctx context.Context,
		conditionKind rctypes.Kind,
		conditionID,
		serverID uuid.UUID,
	) (*rctypes.Task[any, any], error)
	publish(
		ctx context.Context,
		serverID,
		conditionID string,
		conditionKind rctypes.Kind,
		task *rctypes.Task[any, any],
		onlyTimestamp bool,
	) error
}

// implements the taskKV interface
type taskKVImpl struct {
	logger       *logrus.Logger
	kv           nats.KeyValue
	facilityCode string
}

func initTaskKVImpl(facilityCode string, l *logrus.Logger, stream events.Stream) (taskKV, error) {
	njs, ok := stream.(*events.NatsJetstream)
	if !ok {
		panic("bad stream implementation")
	}

	bucket, err := kv.CreateOrBindKVBucket(njs, rctypes.TaskKVRepositoryBucket, kv.WithTTL(10*24*time.Hour))
	if err != nil {
		return nil, err
	}

	return &taskKVImpl{
		logger:       l,
		kv:           bucket,
		facilityCode: facilityCode,
	}, nil
}

func (t *taskKVImpl) get(ctx context.Context, conditionKind rctypes.Kind, conditionID, serverID uuid.UUID) (*rctypes.Task[any, any], error) {
	_, span := otel.Tracer(pkgName).Start(ctx, "taskKV.get")
	defer span.End()

	key := rctypes.TaskKVRepositoryKey(t.facilityCode, conditionKind, serverID.String())
	tle := t.logger.WithFields(logrus.Fields{
		"serverID":     serverID,
		"facilityCode": t.facilityCode,
		"conditionID":  conditionID,
		"key":          key,
	})

	fail := func(err error) (*rctypes.Task[any, any], error) {
		tle.WithError(err).Warn("Task query error")
		span.AddEvent("Task query error",
			trace.WithAttributes(
				attribute.String("serverID", serverID.String()),
				attribute.String("conditionID", conditionID.String()),
				attribute.String("error", err.Error()),
				attribute.String("key", key),
			),
		)

		return nil, err
	}

	currEntry, err := t.kv.Get(key)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return fail(errors.Wrap(errNoTask, err.Error()))
		}

		return fail(errors.Wrap(errQueryTask, err.Error()))
	}

	task, err := rctypes.TaskFromMessage(currEntry.Value())
	if err != nil {
		return fail(errors.Wrap(errQueryTask, err.Error()))
	}

	if task.ID != conditionID {
		return fail(
			errors.Wrap(errStaleTask,
				fmt.Sprintf(
					"active/pending ConditionID: %s does not match current TaskID: %s",
					conditionID,
					task.ID,
				),
			),
		)
	}

	return task, nil
}

func (t *taskKVImpl) publish(
	ctx context.Context,
	serverID,
	conditionID string,
	conditionKind rctypes.Kind,
	task *rctypes.Task[any, any],
	onlyTimestamp bool,
) error {
	_, span := otel.Tracer(pkgName).Start(
		ctx,
		"controller.Publish.KV.Task",
		trace.WithSpanKind(trace.SpanKindConsumer),
	)
	defer span.End()

	key := rctypes.TaskKVRepositoryKey(t.facilityCode, conditionKind, serverID)
	failed := func(err error) error {
		span.AddEvent("Task publish error",
			trace.WithAttributes(
				attribute.String("serverID", serverID),
				attribute.String("conditionID", conditionID),
				attribute.String("error", err.Error()),
				attribute.String("key", key),
			),
		)

		t.logger.WithError(err).WithFields(logrus.Fields{
			"serverID":     serverID,
			"facilityCode": t.facilityCode,
			"conditionID":  conditionID,
			"key":          key,
		}).Warn("Task publish error")

		return errors.Wrap(errPublishTask, err.Error())
	}

	// create
	create := func() error {
		task.CreatedAt = time.Now()
		taskJSON, errMarshal := task.Marshal()
		if errMarshal != nil {
			return failed(errMarshal)
		}

		// nolint:ineffassign // false positive
		rev, errCreate := t.kv.Create(key, taskJSON)
		if errCreate != nil {
			return failed(errCreate)
		}

		t.logger.WithFields(logrus.Fields{
			"serverID":     serverID,
			"facilityCode": t.facilityCode,
			"taskID":       conditionID,
			"rev":          rev,
			"key":          key,
		}).Trace("Task record created")

		return nil
	}

	curr, err := t.kv.Get(key)
	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return create()
		}

		return failed(err)
	}

	currTask, err := rctypes.TaskFromMessage(curr.Value())
	if err != nil {
		return failed(errors.Wrap(err, "Task object deserialize error"))
	}

	if onlyTimestamp {
		currTask.UpdatedAt = time.Now()
	} else {
		// clear stale task entry
		if rctypes.StateIsComplete(currTask.State) ||
			(time.Since(currTask.UpdatedAt) > rctypes.StaleThreshold && currTask.ID != task.ID) {
			if errDelete := t.kv.Delete(key); errDelete != nil {
				return errors.Wrap(errPublishTask, errDelete.Error())
			}

			return create()
		}

		if errUpdate := currTask.Update(task); errUpdate != nil {
			return failed(errUpdate)
		}
	}

	currTaskJSON, err := currTask.Marshal()
	if err != nil {
		return failed(err)
	}

	if _, err := t.kv.Update(key, currTaskJSON, curr.Revision()); err != nil {
		return failed(err)
	}

	t.logger.WithFields(logrus.Fields{
		"serverID":     serverID,
		"facilityCode": t.facilityCode,
		"taskID":       conditionID,
		"rev":          curr.Revision(),
		"key":          key,
		"create":       create,
	}).Trace("Task record updated")

	return nil
}
