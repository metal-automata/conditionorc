package status

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/metal-automata/rivets/events"
	"github.com/nats-io/nats-server/v2/server"
	srvtest "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	rctypes "github.com/metal-automata/rivets/condition"
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

func jetStreamContext(t *testing.T, s *server.Server) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatalf("connect => %v", err)
	}
	js, err := nc.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		t.Fatalf("JetStream => %v", err)
	}
	return nc, js
}

func shutdownJetStream(t *testing.T, s *server.Server) {
	t.Helper()
	var sd string
	if config := s.JetStreamConfig(); config != nil {
		sd = config.StoreDir
	}
	s.Shutdown()
	if sd != "" {
		if err := os.RemoveAll(sd); err != nil {
			t.Fatalf("Unable to remove storage %q: %v", sd, err)
		}
	}
	s.WaitForShutdown()
}

type shutdownFunc func()

func startTestJetStream(t *testing.T) (*events.NatsJetstream, shutdownFunc) {
	t.Helper()
	srv := startJetStreamServer(t)
	conn, _ := jetStreamContext(t, srv)
	evJS := events.NewJetstreamFromConn(conn)
	return evJS, func() {
		evJS.Close()
		shutdownJetStream(t, srv)
	}
}

func TestStatusKV(t *testing.T) {
	t.Parallel()
	js, testDone := startTestJetStream(t)
	defer testDone()

	defs := rctypes.Definitions{
		&rctypes.Definition{
			Kind: rctypes.Kind("test-event"),
		},
	}

	// pre-ready returns an error
	require.False(t, kvReady)
	_, err := WatchConditionStatus(context.TODO(), rctypes.Kind("bogus"), "my-facility")
	require.ErrorIs(t, err, errNotReady, "wrong error")

	_, err = GetConditionKV(rctypes.Kind("bogus"))
	require.ErrorIs(t, err, errNotReady, "wrong error")

	ConnectToKVStores(js, &logrus.Logger{}, defs)
	require.True(t, kvReady)

	// bogus condition name returns an error
	_, err = WatchConditionStatus(context.TODO(), rctypes.Kind("bogus"), "my-facility")
	require.ErrorIs(t, err, errNoKV, "wrong error")

	_, err = GetConditionKV(rctypes.Kind("bogus"))
	require.ErrorIs(t, err, errNoKV, "wrong error")

	// use the configured kind and get a real object back
	_, err = WatchConditionStatus(context.TODO(), rctypes.Kind("test-event"), "my-facility")
	require.NoError(t, err)

	testKind := rctypes.Kind("test-event")
	handle, err := GetConditionKV(testKind)
	require.NoError(t, err)

	// write some data to the KV store to exercise our get methods
	_, err = handle.PutString("badkeyformat", "bogus data")
	require.NoError(t, err)
	_, err = handle.PutString("fc13.firstkey", "This is some fc13 data")
	require.NoError(t, err)
	_, err = handle.PutString("fc1.firstkey", "This is some fc1 data")
	require.NoError(t, err)
	_, err = handle.PutString("fc13.secondkey", "This is more fc13 data")
	require.NoError(t, err)

	c, err := GetSingleCondition(testKind, "fc1", "firstkey")
	require.NotNil(t, c)
	require.NoError(t, err)

	es, err := GetAllConditions(testKind, "fc13")
	require.NoError(t, err)
	require.Equal(t, 2, len(es))

	// Delete fc13.secondkey and it's gone
	err = DeleteCondition(testKind, "fc13", "secondkey")
	require.NoError(t, err)
	c, err = GetSingleCondition(testKind, "fc13", "secondkey")
	require.Nil(t, c)
	require.ErrorIs(t, err, nats.ErrKeyNotFound)
	// delete something that isn't there and it's not an error
	err = DeleteCondition(testKind, "fc13", "secondkey")
	require.NoError(t, err)

	es, err = GetAllConditions(testKind, "fc8")
	require.NoError(t, err)
	require.Equal(t, 0, len(es))
}
