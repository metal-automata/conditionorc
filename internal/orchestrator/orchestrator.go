package orchestrator

import (
	"context"
	"sync"

	"github.com/metal-automata/conditionorc/internal/fleetdb"
	"github.com/metal-automata/conditionorc/internal/orchestrator/notify"
	"github.com/metal-automata/conditionorc/internal/store"
	"github.com/metal-automata/conditionorc/internal/version"
	"github.com/metal-automata/rivets/events"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	rctypes "github.com/metal-automata/rivets/condition"
)

var (
	concurrency     = 10
	ErrPublishEvent = errors.New("error publishing event")
	ErrInvalidEvent = errors.New("invalid event message")
	pkgName         = "internal/orchestrator"
)

// Orchestrator type holds attributes of the condition orchestrator service
type Orchestrator struct {
	// Logger is the app logger
	logger        *logrus.Logger
	listenAddress string
	concurrency   int
	repository    store.Repository
	streamBroker  events.Stream
	replicaCount  int
	notifier      notify.Sender
	facility      string
	conditionDefs rctypes.Definitions
	db            fleetdb.FleetDB
}

// Option type sets a parameter on the Orchestrator type.
type Option func(*Orchestrator)

// WithStore sets the storage repository on the Orchestrator type.
func WithStore(repository store.Repository) Option {
	return func(o *Orchestrator) {
		o.repository = repository
	}
}

// WithStreamBroker sets the event stream broker on the Orchestrator type.
func WithStreamBroker(broker events.Stream) Option {
	return func(o *Orchestrator) {
		o.streamBroker = broker
	}
}

// WithLogger sets the logger on the Orchestrator type.
func WithLogger(logger *logrus.Logger) Option {
	return func(o *Orchestrator) {
		o.logger = logger
	}
}

// WithListenAddress sets the Orchestrator listen address - for health checks.
func WithListenAddress(addr string) Option {
	return func(o *Orchestrator) {
		o.listenAddress = addr
	}
}

// WithConcurrency sets the Orchestrator event concurrency, defaults to 1.
func WithConcurrency(c int) Option {
	return func(o *Orchestrator) {
		o.concurrency = c
	}
}

// WithReplicas sets the number of replicas we'll use when instaintiating the NATS
// liveness and status KV buckets. This is only used in the rare case when the buckets
// do not already exist (e.g. when operating in the sandbox environment).
func WithReplicas(c int) Option {
	return func(o *Orchestrator) {
		o.replicaCount = c
	}
}

// WithNotifier sets a notifier for condition state transition updates
func WithNotifier(s notify.Sender) Option {
	return func(o *Orchestrator) {
		o.notifier = s
	}
}

// WithFacility sets a site-specific descriptor to focus the orchestrator's work.
func WithFacility(f string) Option {
	return func(o *Orchestrator) {
		o.facility = f
	}
}

// WithConditionDefs sets the configured condition definitions where the orchestrator
// can access them at runtime.
func WithConditionDefs(defs rctypes.Definitions) Option {
	return func(o *Orchestrator) {
		o.conditionDefs = defs
	}
}

// WithFleetDBClient sets the database client to use on this Orchestrator
func WithFleetDBClient(client fleetdb.FleetDB) Option {
	return func(o *Orchestrator) {
		o.db = client
	}
}

// New returns a new orchestrator service with the given options set.
func New(opts ...Option) *Orchestrator {
	o := &Orchestrator{concurrency: concurrency}

	for _, opt := range opts {
		opt(o)
	}

	return o
}

// Run runs the orchestrator which listens for events to action.
func (o *Orchestrator) Run(ctx context.Context) {
	wg := &sync.WaitGroup{}

	v := version.Current()
	o.logger.WithFields(logrus.Fields{
		"GitCommit":  v.GitCommit,
		"GitBranch":  v.GitBranch,
		"AppVersion": v.AppVersion,
	}).Info("running orchestrator")
	o.startWorkerLivenessCheckin(ctx)
	o.startUpdateMonitor(ctx)
	o.startReconciler(ctx, wg)

	<-ctx.Done()
	wg.Wait()
	o.streamBroker.Close()
	o.logger.Info("orchestrator shut down")
}
