// nolint
package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/fleetdb"
	"github.com/metal-automata/conditionorc/internal/store"
	"github.com/metal-automata/conditionorc/pkg/api/v1/orchestrator/routes"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/orchestrator/types"
	rctypes "github.com/metal-automata/rivets/condition"
	eventsm "github.com/metal-automata/rivets/events"
)

const (
	facility = "foo13"
)

// tester holds all the mocks for easier passing around
type tester struct {
	mockStore         *store.MockRepository
	mockFleetDB       *fleetdb.MockFleetDB
	mockStream        *eventsm.MockStream
	mockStatusValueKV *routes.MockstatusValueKV
	mocktaskKV        *routes.MocktaskKV
}

func mockrouter(t *testing.T, mtester *tester) (*gin.Engine, error) {
	t.Helper()

	gin.SetMode(gin.ReleaseMode)
	g := gin.New()
	g.Use(gin.Recovery())

	options := []routes.Option{
		routes.WithLogger(logrus.New()),
		routes.WithStore(mtester.mockStore),
		routes.WithFleetDBClient(mtester.mockFleetDB),
		routes.WithStatusKVPublisher(mtester.mockStatusValueKV),
		routes.WithTaskKV(mtester.mocktaskKV),
		routes.WithConditionDefinitions(
			[]*rctypes.Definition{
				{Kind: rctypes.FirmwareInstall},
			},
		),
		routes.WithFacilityCode(facility),
	}

	if mtester.mockStream != nil {
		options = append(options, routes.WithStreamBroker(mtester.mockStream, "foo"))
	}

	v1Router, err := routes.NewRoutes(options...)
	if err != nil {
		return nil, err
	}

	v1Router.Routes(g.Group("/api/v1"))

	g.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"message": "invalid request - route not found"})
	})

	return g, nil
}

func setupMockRouter(t *testing.T) (*tester, *gin.Engine, error) {
	mtester := &tester{
		mockStore:         store.NewMockRepository(t),
		mockFleetDB:       fleetdb.NewMockFleetDB(t),
		mockStream:        eventsm.NewMockStream(t),
		mockStatusValueKV: routes.NewMockstatusValueKV(t),
		mocktaskKV:        routes.NewMocktaskKV(t),
	}

	r, err := mockrouter(t, mtester)
	return mtester, r, err
}

func TestConditionStatusUpdate(t *testing.T) {
	serverID := uuid.New()
	conditionID := uuid.New()
	cond := &rctypes.Condition{
		ID:   conditionID,
		Kind: rctypes.FirmwareInstall,
	}

	mtester, server, err := setupMockRouter(t)
	if err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name                string
		statusValue         *rctypes.StatusValue
		onlyUpdateTimestamp bool
		mockStore           func(r *store.MockRepository)
		mockKVPublisher     func(p *routes.MockstatusValueKV)
		expectResponse      func() *v1types.ServerResponse
	}{
		{
			name: "successful update",
			statusValue: &rctypes.StatusValue{
				State:  string(rctypes.Active),
				Status: json.RawMessage(`{"message":"Updating firmware"}`),
			},
			onlyUpdateTimestamp: false,
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: conditionID, Kind: rctypes.FirmwareInstall, State: rctypes.Active},
						nil,
					).
					Once()
			},
			mockKVPublisher: func(p *routes.MockstatusValueKV) {
				p.On("publish", facility, cond.ID, cond.Kind, mock.Anything, false).
					Return(nil).
					Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					Message:    "condition status update published",
					StatusCode: http.StatusOK,
				}
			},
		},
		{
			name:                "timestamp update only",
			statusValue:         nil,
			onlyUpdateTimestamp: true,
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: conditionID, Kind: rctypes.FirmwareInstall, State: rctypes.Active},
						nil,
					).
					Once()
			},
			mockKVPublisher: func(p *routes.MockstatusValueKV) {
				p.On("publish", facility, cond.ID, cond.Kind, mock.Anything, true).
					Return(nil).
					Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					Message:    "condition status update published",
					StatusCode: http.StatusOK,
				}
			},
		},
		{
			name:                "no active condition",
			statusValue:         nil,
			onlyUpdateTimestamp: false,
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: conditionID, Kind: rctypes.Kind("other_kind"), State: rctypes.Active},
						nil,
					).
					Once()
			},
			mockKVPublisher: nil,
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					Message:    "no matching condition found in record: firmwareInstall",
					StatusCode: http.StatusBadRequest,
				}
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mockStore != nil {
				tc.mockStore(mtester.mockStore)
			}
			if tc.mockKVPublisher != nil {
				tc.mockKVPublisher(mtester.mockStatusValueKV)
			}

			testServer := httptest.NewServer(server)
			defer testServer.Close()

			client := &Client{
				serverAddress: testServer.URL,
				client:        http.DefaultClient,
			}

			got, err := client.ConditionStatusUpdate(
				context.TODO(),
				cond.Kind,
				serverID,
				conditionID,
				tc.statusValue,
				tc.onlyUpdateTimestamp,
			)
			require.NoError(t, err)

			if tc.expectResponse != nil {
				assert.Equal(t, tc.expectResponse(), got)
			}
		})
	}
}

func TestConditionTaskPublish(t *testing.T) {
	serverID := uuid.New()
	conditionID := uuid.New()
	conditionKind := rctypes.FirmwareInstall
	task := &rctypes.Task[any, any]{
		ID:   conditionID,
		Kind: rctypes.FirmwareInstall,
	}

	mtester, server, err := setupMockRouter(t)
	if err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name                string
		task                *rctypes.Task[any, any]
		onlyUpdateTimestamp bool
		mockStore           func(r *store.MockRepository)
		mockTaskKV          func(tk *routes.MocktaskKV)
		expectResponse      func() *v1types.ServerResponse
	}{
		{
			name:                "successful task publish",
			task:                task,
			onlyUpdateTimestamp: false,
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(&rctypes.Condition{ID: conditionID, Kind: conditionKind}, nil).
					Once()
			},
			mockTaskKV: func(tk *routes.MocktaskKV) {
				tk.On("publish", mock.Anything, serverID.String(), conditionID.String(), conditionKind, mock.IsType(&rctypes.Task[any, any]{}), false).
					Return(nil).
					Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					Message:    "condition Task published",
					StatusCode: http.StatusOK,
				}
			},
		},
		{
			name:                "timestamp update only",
			task:                nil,
			onlyUpdateTimestamp: true,
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(&rctypes.Condition{ID: conditionID, Kind: conditionKind}, nil).
					Once()
			},
			mockTaskKV: func(tk *routes.MocktaskKV) {
				tk.On("publish", mock.Anything, serverID.String(), conditionID.String(), conditionKind, mock.IsType(&rctypes.Task[any, any]{}), true).
					Return(nil).
					Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					Message:    "condition Task published",
					StatusCode: http.StatusOK,
				}
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tc.mockStore(mtester.mockStore)
			tc.mockTaskKV(mtester.mocktaskKV)

			testServer := httptest.NewServer(server)
			defer testServer.Close()

			client := &Client{
				serverAddress: testServer.URL,
				client:        http.DefaultClient,
			}

			got, err := client.ConditionTaskPublish(context.TODO(), conditionKind, serverID, conditionID, tc.task, tc.onlyUpdateTimestamp)

			require.NoError(t, err)
			assert.Equal(t, tc.expectResponse(), got)
		})
	}
}

func TestConditionTaskQuery(t *testing.T) {
	serverID := uuid.New()
	conditionID := uuid.New()
	conditionKind := rctypes.FirmwareInstall
	task := &rctypes.Task[any, any]{
		ID:   conditionID,
		Kind: conditionKind,
	}

	mtester, server, err := setupMockRouter(t)
	if err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name           string
		mockStore      func(r *store.MockRepository)
		mockTaskKV     func(tk *routes.MocktaskKV)
		expectResponse func() *v1types.ServerResponse
	}{
		{
			name: "successful task query",
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(&rctypes.Condition{ID: conditionID, Kind: conditionKind}, nil).
					Once()
			},
			mockTaskKV: func(tk *routes.MocktaskKV) {
				tk.On("get", mock.Anything, conditionKind, conditionID, serverID).
					Return(task, nil).
					Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					Message:    "Task identified",
					Task:       task,
					StatusCode: http.StatusOK,
				}
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tc.mockStore(mtester.mockStore)
			tc.mockTaskKV(mtester.mocktaskKV)

			testServer := httptest.NewServer(server)
			defer testServer.Close()

			client := &Client{
				serverAddress: testServer.URL,
				client:        http.DefaultClient,
			}

			got, err := client.ConditionTaskQuery(context.TODO(), conditionKind, serverID)

			require.NoError(t, err)
			assert.Equal(t, tc.expectResponse(), got)
		})
	}
}
