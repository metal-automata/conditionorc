// nolint
package notify

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/conditions/types"
	rctypes "github.com/metal-automata/rivets/condition"
)

// cribbed this test infra from the slack API open source code
var (
	serverAddr string
	once       sync.Once
)

func startServer() {
	server := httptest.NewServer(nil)
	serverAddr = server.Listener.Addr().String()
}

func TestSlackSend(t *testing.T) {
	var postCalled, updateCalled bool
	http.DefaultServeMux = new(http.ServeMux)
	http.HandleFunc("/chat.postMessage", func(rw http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		r.Body.Close()
		rw.Header().Set("Content-Type", "application/json")
		response := []byte("{\"ok\": true, \"ts\": \"timestamp\"}")
		rw.Write(response)
		postCalled = true
	})
	http.HandleFunc("/chat.update", func(rw http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		r.Body.Close()
		rw.Header().Set("Content-Type", "application/json")
		response := []byte("{\"ok\": true, \"ts\": \"new_timestamp\"}")
		rw.Write(response)
		updateCalled = true
	})

	once.Do(startServer)

	notifier := &slackSender{
		api: slack.New("totally-valid-token", slack.OptionAPIURL("http://"+serverAddr+"/")),
		log: &logrus.Logger{},
		trk: make(map[uuid.UUID]slackNotification),
		ch:  "#bogus",
	}

	condID := uuid.New()
	update := &v1types.ConditionUpdateEvent{
		ConditionUpdate: v1types.ConditionUpdate{
			ConditionID: condID,
			ServerID:    uuid.New(),
			State:       rctypes.Pending,
			Status:      []byte(`{ "msg":"Hi Vince!" }`),
		},
		Kind: rctypes.FirmwareInstall,
	}

	err := notifier.Send(update)
	require.NoError(t, err)
	entry, ok := notifier.trk[condID]
	require.True(t, ok)
	require.Equal(t, string(rctypes.Pending), entry.ConditionState) // weak test >.>;
	require.True(t, postCalled)

	update.State = rctypes.Failed // oh no! :(
	err = notifier.Send(update)
	require.NoError(t, err)
	entry, ok = notifier.trk[condID]
	require.False(t, ok)
	require.True(t, updateCalled)
}
