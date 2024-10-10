package status

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/metal-automata/rivets/events"
	"github.com/metal-automata/rivets/events/pkg/kv"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"

	rctypes "github.com/metal-automata/rivets/condition"
)

var (
	statusOnce   sync.Once
	statusTTL    = 10 * 24 * time.Hour // expire status information in 10 days
	kvCollection = map[string]nats.KeyValue{}
	errNotReady  = errors.New("condition kvs not initialized")
	errNoKV      = errors.New("no kv for condition")
	kvReady      bool
)

// ConnectToKVStores initializes all status KVs in preparation for monitoring status updates
// Any errors here are fatal, as we are failing to initialize something we are explicitly
// configured for.
func ConnectToKVStores(s events.Stream, log *logrus.Logger,
	defs rctypes.Definitions, opts ...kv.Option,
) {
	js, ok := s.(*events.NatsJetstream)
	if !ok {
		log.Fatal("status via KV updates is only supported on NATS")
	}

	statusOpts := []kv.Option{
		kv.WithTTL(statusTTL),
	}

	if len(opts) > 0 {
		statusOpts = append(statusOpts, opts...)
	}

	statusOnce.Do(func() {
		for _, def := range defs {
			kind := string(def.Kind)
			hdl, err := kv.CreateOrBindKVBucket(js, kind, statusOpts...)
			if err != nil {
				log.WithError(err).
					WithField("kv.type", kind).
					Fatal("unable to initialize NATS KV for status")
			}
			kvCollection[kind] = hdl
		}
		kvReady = true
	})
}

func getKVBucket(kind rctypes.Kind) (nats.KeyValue, error) {
	if !kvReady {
		return nil, errNotReady
	}

	bucket, ok := kvCollection[string(kind)]
	if !ok {
		return nil, errors.Wrap(errNoKV, string(kind))
	}
	return bucket, nil
}

// WatchConditionStatus specializes some generic NATS functionality, mainly to keep
// the callers cleaner of the NATS-specific details.
func WatchConditionStatus(ctx context.Context, kind rctypes.Kind, facility string) (nats.KeyWatcher, error) {
	bucket, err := getKVBucket(kind)
	if err != nil {
		return nil, err
	}

	// format the facility as a NATS subject to use as a filter for relevant KVs
	keyStr := fmt.Sprintf("%s.*", facility)
	return bucket.Watch(keyStr, nats.Context(ctx), nats.IgnoreDeletes())
}

// GetConditionKV returns the raw NATS KeyValue interface for the bucket associated
// with the given condition type. This is a really low-level access, but if you want
// a handle to the raw NATS API, here it is.
func GetConditionKV(kind rctypes.Kind) (nats.KeyValue, error) {
	bucket, err := getKVBucket(kind)
	if err != nil {
		return nil, err
	}

	return bucket, nil
}

// DeleteCondition does what it says on the tin. If this does not return an error, the
// KV entry is gone.
func DeleteCondition(kind rctypes.Kind, facility, condID string) error {
	bucket, err := getKVBucket(kind)
	if err != nil {
		return err
	}

	return bucket.Delete(facility + "." + condID)
}

// GetSingleCondition does exactly that given a kind, facility, and condition-id
func GetSingleCondition(kind rctypes.Kind, facility, condID string) (nats.KeyValueEntry, error) {
	bucket, err := getKVBucket(kind)
	if err != nil {
		return nil, err
	}

	entry, err := bucket.Get(facility + "." + condID)
	if err != nil {
		return nil, err
	}

	return entry, nil
}

// GetAllConditions returns all conditions for a specific type and facility. This includes any
// entry in any state, provided it has not been reaped by TTL.
func GetAllConditions(kind rctypes.Kind, facility string) ([]nats.KeyValueEntry, error) {
	bucket, err := getKVBucket(kind)
	if err != nil {
		return nil, err
	}

	// instead of making multiple calls (one to Keys() and then multiple ones to Get(),
	// do what they do in the NATS code and use a watcher to get everything from the server
	// in one shot.

	facilityOnlyKey := fmt.Sprintf("%s.*", facility)

	watcher, err := bucket.Watch(facilityOnlyKey, nats.IgnoreDeletes())
	if err != nil {
		return nil, errors.Wrap(err, "GetAllConditions::"+string(kind)+"::"+facility)
	}
	//nolint:errcheck,gocritic
	defer watcher.Stop()

	conds := []nats.KeyValueEntry{}

	for kve := range watcher.Updates() {
		if kve == nil {
			// this is weird, and it's also in their code. The channel isn't closed
			// until the internal watcher's subscription to the KV subject is shut down
			// so getting an explicit nil here means "nothing more."
			break
		}
		conds = append(conds, kve)
	}
	return conds, nil
}
