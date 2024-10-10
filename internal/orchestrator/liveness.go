package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/metal-automata/rivets/events"
	"github.com/metal-automata/rivets/events/pkg/kv"
	"github.com/metal-automata/rivets/events/registry"

	"github.com/metal-automata/conditionorc/internal/metrics"
	"github.com/nats-io/nats.go"
)

var (
	livOnce        sync.Once
	checkinCadence = 30 * time.Second
	livenessTTL    = 3 * time.Minute
)

// This starts a go-routine to peridocally check in with the NATS kv
func (o *Orchestrator) startWorkerLivenessCheckin(ctx context.Context) {
	livOnce.Do(func() {
		natsJS, ok := o.streamBroker.(*events.NatsJetstream)
		if !ok {
			o.logger.Error("Non-NATS stores are not supported for worker-liveness")
			return
		}

		opts := []kv.Option{
			kv.WithTTL(livenessTTL),
		}

		// only set replicas if we need them
		if o.replicaCount > 1 {
			opts = append(opts, kv.WithReplicas(o.replicaCount))
		}

		if err := registry.InitializeRegistryWithOptions(natsJS, opts...); err != nil {
			o.logger.WithError(err).Error("unable to initialize active worker registry")
			return
		}

		go o.checkinRoutine(ctx)
	})
}

func (o *Orchestrator) checkinRoutine(ctx context.Context) {
	workerName := fmt.Sprintf("orchestrator-%s", o.facility)
	me := registry.GetID(workerName)
	o.logger.WithField("id", me.String()).Info("worker id assigned")
	if err := registry.RegisterController(me); err != nil {
		metrics.DependencyError("liveness", "register")
		o.logger.WithError(err).WithField("id", me.String()).
			Warn("unable to do initial worker liveness registration")
	}

	tick := time.NewTicker(checkinCadence)
	defer tick.Stop()

	var stop bool
	for !stop {
		select {
		case <-tick.C:
			err := registry.ControllerCheckin(me)
			if err != nil {
				metrics.DependencyError("liveness", "check-in")
				o.logger.WithError(err).WithField("id", me.String()).Warn("worker checkin failed")
				// try to refresh our token, maybe this is a NATS hiccup
				err = refreshWorkerToken(me)
				if err != nil {
					// couldn't refresh our liveness token, time to bail out
					o.logger.WithError(err).WithField("id", me.String()).Fatal("reregister this worker")
				}
			}
		case <-ctx.Done():
			o.logger.Info("liveness check-in stopping on done context")
			stop = true
		}
	}
}

// try to de-register/re-register this id.
func refreshWorkerToken(id registry.ControllerID) error {
	err := registry.DeregisterController(id)
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		metrics.DependencyError("liveness", "de-register")
		return err
	}
	err = registry.RegisterController(id)
	if err != nil {
		metrics.DependencyError("liveness", "register")
		return err
	}
	return nil
}
