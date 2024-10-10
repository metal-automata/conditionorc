// nolint
package store

import (
	"context"
	"os"
	"testing"
	"time"

	rctypes "github.com/metal-automata/rivets/condition"

	"github.com/google/uuid"
	"github.com/metal-automata/rivets/events"
	"github.com/metal-automata/rivets/events/pkg/kv"
	"github.com/nats-io/nats-server/v2/server"
	srvtest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

var (
	nc     *nats.Conn
	js     nats.JetStreamContext
	evJS   *events.NatsJetstream
	logger *logrus.Logger
	bucket nats.KeyValue
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

// do some one-time setup
func TestMain(m *testing.M) {
	logger = logrus.New()

	t := &testing.T{}
	srv := startJetStreamServer(t)
	defer shutdownJetStream(t, srv)
	nc, js = jetStreamContext(srv) // nc is closed on evJS.Close(), js needs no cleanup
	evJS = events.NewJetstreamFromConn(nc)
	defer evJS.Close()

	// initialize the bucket here
	var err error
	bucket, err = kv.CreateOrBindKVBucket(evJS, ActiveConditionBucket)
	if err != nil {
		logger.WithError(err).Fatal("kv setup")
	}

	exitCode := m.Run()
	os.Exit(exitCode)
}

func TestCreateReadUpdate(t *testing.T) {
	t.Parallel()
	serverID := uuid.New()

	kind := rctypes.Kind("test-kind")

	condition := &rctypes.Condition{
		ID:    uuid.New(),
		Kind:  kind,
		State: rctypes.Pending,
	}

	logger := &logrus.Logger{}

	store := &natsStore{
		bucket: bucket,
		log:    logger,
	}

	// look for a condition in an empty bucket and find nothing
	_, err := store.Get(context.TODO(), serverID)
	require.ErrorIs(t, err, ErrConditionNotFound)

	active, err := store.GetActiveCondition(context.TODO(), serverID)
	require.ErrorIs(t, err, ErrConditionNotFound)
	require.Nil(t, active)

	// add a condition
	err = store.Create(context.TODO(), serverID, "fc-13", condition)
	require.NoError(t, err)

	active, err = store.GetActiveCondition(context.TODO(), serverID)
	require.NoError(t, err)
	require.NotNil(t, active)

	// get the new condition
	cr, err := store.Get(context.TODO(), serverID)
	require.NoError(t, err)
	require.Equal(t, condition.ID.String(), cr.ID.String())
	require.Equal(t, "fc-13", cr.Facility)

	// update the condition
	condition.State = rctypes.Active
	err = store.Update(context.TODO(), serverID, condition)
	require.NoError(t, err)

	// read the new condition
	cr, err = store.Get(context.TODO(), serverID)
	require.NoError(t, err)
	require.Equal(t, rctypes.Active, cr.State)

	condition.State = rctypes.Succeeded
	err = store.Update(context.TODO(), serverID, condition)
	require.NoError(t, err)

	active, err = store.GetActiveCondition(context.TODO(), serverID)
	require.ErrorIs(t, err, ErrConditionNotFound)
	require.Nil(t, active)

	cr, err = store.Get(context.TODO(), serverID)
	require.NoError(t, err)
	require.Equal(t, rctypes.Succeeded, cr.State)
}

// Given a conditionRecord with multiple conditions, walk through some common
// scenarios around Create/Update/Get/GetActive
func TestMultipleConditionUpdate(t *testing.T) {
	t.Parallel()

	facility := "fac13"
	logger := &logrus.Logger{}

	store := &natsStore{
		bucket: bucket,
		log:    logger,
	}
	t.Run("create multiple sanity checks", func(t *testing.T) {
		serverID := uuid.New()

		err := store.Create(context.TODO(), serverID, facility)
		require.NoError(t, err, "created multiple with nil work")

		work := []*rctypes.Condition{
			{
				Kind:  rctypes.Kind("first"),
				State: rctypes.Pending,
			},
		}

		err = store.Create(context.TODO(), serverID, facility, work...)
		require.NoError(t, err, "created multiple on idle server with work")

		err = store.Create(context.TODO(), serverID, facility, work...)
		require.ErrorIs(t, err, ErrActiveCondition, "created multiple on busy server with work")

	})
	t.Run("success path", func(t *testing.T) {
		serverID := uuid.New()
		first := &rctypes.Condition{
			Kind:  rctypes.Kind("first"),
			State: rctypes.Pending,
		}
		second := &rctypes.Condition{
			Kind:  rctypes.Kind("second"),
			State: rctypes.Pending,
		}
		work := []*rctypes.Condition{
			first,
			second,
		}

		err := store.Create(context.TODO(), serverID, facility, work...)
		require.NoError(t, err, "Create")

		active, err := store.GetActiveCondition(context.TODO(), serverID)
		require.NoError(t, err, "GetActiveCondition I")
		require.Equal(t, active, first)

		first.State = rctypes.Active
		err = store.Update(context.TODO(), serverID, first)
		require.NoError(t, err, "first update - active")

		active, err = store.GetActiveCondition(context.TODO(), serverID)
		require.NoError(t, err, "GetActiveCondition II")
		require.Equal(t, active, first)

		first.State = rctypes.Succeeded
		err = store.Update(context.TODO(), serverID, first)
		require.NoError(t, err, "first update - succeeded")

		active, err = store.GetActiveCondition(context.TODO(), serverID)
		require.NoError(t, err, "GetActiveCondition III")
		require.Equal(t, active, second)

		second.State = rctypes.Succeeded
		err = store.Update(context.TODO(), serverID, second)
		require.NoError(t, err, "second update - succeeded")

		active, err = store.GetActiveCondition(context.TODO(), serverID)
		require.ErrorIs(t, err, ErrConditionNotFound, "GetActiveCondition IV")
		require.Nil(t, active)

		cr, err := store.Get(context.TODO(), serverID)
		require.NoError(t, err)
		require.Equal(t, rctypes.Succeeded, cr.State)
	})
	t.Run("failure short circuit", func(t *testing.T) {
		serverID := uuid.New()
		first := &rctypes.Condition{
			Kind:  rctypes.Kind("first"),
			State: rctypes.Pending,
		}
		second := &rctypes.Condition{
			Kind:  rctypes.Kind("second"),
			State: rctypes.Pending,
		}
		work := []*rctypes.Condition{
			first,
			second,
		}

		err := store.Create(context.TODO(), serverID, facility, work...)
		require.NoError(t, err, "Create")

		active, err := store.GetActiveCondition(context.TODO(), serverID)
		require.NoError(t, err, "GetActiveCondition I")
		require.Equal(t, active, first)

		first.State = rctypes.Active
		err = store.Update(context.TODO(), serverID, first)
		require.NoError(t, err, "first update - active")

		active, err = store.GetActiveCondition(context.TODO(), serverID)
		require.NoError(t, err, "GetActiveCondition II")
		require.Equal(t, active, first)

		first.State = rctypes.Failed
		err = store.Update(context.TODO(), serverID, first)
		require.NoError(t, err, "first update - failed")

		active, err = store.GetActiveCondition(context.TODO(), serverID)
		require.ErrorIs(t, err, ErrConditionNotFound, "GetActiveCondition III")
		require.Nil(t, active)

		cr, err := store.Get(context.TODO(), serverID)
		require.NoError(t, err)
		require.Equal(t, rctypes.Failed, cr.State)
	})
}
