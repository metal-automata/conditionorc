// nolint
package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/fleetdb"
	"github.com/metal-automata/conditionorc/internal/store"
	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/orchestrator/types"
	"github.com/pkg/errors"

	rctypes "github.com/metal-automata/rivets/condition"
	eventsm "github.com/metal-automata/rivets/events"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	mock "github.com/stretchr/testify/mock"
)

const (
	facility = "foo13"
)

// tester holds all the mocks for easier passing around
type tester struct {
	mockStore         *store.MockRepository
	mockFleetDB       *fleetdb.MockFleetDB
	mockStream        *eventsm.MockStream
	mockStatusValueKV *MockstatusValueKV
	mocktaskKV        *MocktaskKV
}

func mockserver(t *testing.T, mtester *tester) (*gin.Engine, error) {
	t.Helper()

	gin.SetMode(gin.ReleaseMode)
	g := gin.New()
	g.Use(gin.Recovery())

	options := []Option{
		WithLogger(logrus.New()),
		WithStore(mtester.mockStore),
		WithFleetDBClient(mtester.mockFleetDB),
		WithStatusKVPublisher(mtester.mockStatusValueKV),
		WithTaskKV(mtester.mocktaskKV),
		WithConditionDefinitions(
			[]*rctypes.Definition{
				{Kind: rctypes.FirmwareInstall},
			},
		),
		WithFacilityCode(facility),
	}

	if mtester.mockStream != nil {
		options = append(options, WithStreamBroker(mtester.mockStream, "foo"))
	}

	v1Router, err := NewRoutes(options...)
	if err != nil {
		return nil, err
	}

	v1Router.Routes(g.Group("/api/v1"))

	g.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{"message": "invalid request - route not found"})
	})

	return g, nil
}

func asBytes(t *testing.T, b *bytes.Buffer) []byte {
	t.Helper()

	body, err := io.ReadAll(b)
	if err != nil {
		t.Error(err)
	}

	return body
}

func setupTestServer(t *testing.T) (*tester, *gin.Engine, error) {
	mtester := &tester{
		mockStore:         store.NewMockRepository(t),
		mockFleetDB:       fleetdb.NewMockFleetDB(t),
		mockStream:        eventsm.NewMockStream(t),
		mockStatusValueKV: NewMockstatusValueKV(t),
		mocktaskKV:        NewMocktaskKV(t),
	}

	server, err := mockserver(t, mtester)
	return mtester, server, err
}

func TestConditionStatusUpdate(t *testing.T) {
	serverID := uuid.New()
	conditionID := uuid.New()
	conditionKind := rctypes.FirmwareInstall

	mtester, server, err := setupTestServer(t)
	if err != nil {
		t.Fatal(err)
	}

	surl := fmt.Sprintf("/api/v1/servers/%s/condition-status/%s/%s", serverID, conditionKind, conditionID)

	testcases := []struct {
		name            string
		mockRepository  func(r *store.MockRepository)
		mockKVPublisher func(p *MockstatusValueKV)
		request         func(t *testing.T) *http.Request
		assertResponse  func(t *testing.T, r *httptest.ResponseRecorder)
	}{
		{
			name: "invalid server id",
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPut, fmt.Sprintf("/api/v1/servers/%s/condition-status/%s/%s", "invalid_serverid", conditionKind, conditionID), http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "invalid server id")
			},
		},
		{
			name: "invalid status value payload",
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPut, surl, bytes.NewBufferString(`invalid json`))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "invalid StatusValue payload")
			},
		},
		{
			name: "unsupported condition kind",
			request: func(t *testing.T) *http.Request {
				payload := `{"status": "in_progress", "message": "Updating firmware"}`
				endpoint := fmt.Sprintf("/api/v1/servers/%s/condition-status/%s/%s", serverID, "unsupported_kind", conditionID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPut, endpoint, bytes.NewBufferString(payload))
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "unsupported condition kind")
			},
		},
		{
			name: "no matching condition found",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: conditionID, Kind: rctypes.Kind("other_kind"), State: rctypes.Active},
						nil,
					).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				payload := `{"status": "in_progress", "message": "Updating firmware"}`
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPut, surl, bytes.NewBufferString(payload))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "no matching condition found in record")
			},
		},
		{
			name: "condition not found",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(nil, store.ErrConditionNotFound).Once()
			},
			request: func(t *testing.T) *http.Request {
				payload := `{"status": "in_progress", "message": "Updating firmware"}`
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPut, surl, bytes.NewBufferString(payload))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusNotFound, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), store.ErrConditionNotFound.Error())
			},
		},
		{
			name: "conditionID does not match",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: uuid.New(), Kind: rctypes.FirmwareInstall, State: rctypes.Active},
						nil,
					).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				payload := `{"status": "in_progress", "message": "Updating firmware"}`
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPut, surl, bytes.NewBufferString(payload))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "update denied, condition ID does not match")
			},
		},
		{
			name: "condition in final state",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: conditionID, Kind: rctypes.FirmwareInstall, State: rctypes.Succeeded},
						nil,
					).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				payload := `{"status": "in_progress", "message": "Updating firmware"}`
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPut, surl, bytes.NewBufferString(payload))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "update denied, condition in final state")
			},
		},
		{
			name: "successful update",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: conditionID, Kind: rctypes.FirmwareInstall, State: rctypes.Active},
						nil,
					).
					Once()
			},
			mockKVPublisher: func(p *MockstatusValueKV) {
				p.On("publish", facility, conditionID, conditionKind, mock.Anything, false).
					Return(nil).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				payload := `{"status": "in_progress", "message": "Updating firmware"}`
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPut, surl, bytes.NewBufferString(payload))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "condition status update published")
			},
		},
		{
			name: "timestamp update only",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: conditionID, Kind: rctypes.FirmwareInstall, State: rctypes.Active},
						nil,
					).
					Once()
			},
			mockKVPublisher: func(p *MockstatusValueKV) {
				p.On("publish", facility, conditionID, conditionKind, mock.Anything, true).
					Return(nil).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				endpoint := surl + "?ts_update=true"
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPut, endpoint, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "condition status update published")
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mockRepository != nil {
				tc.mockRepository(mtester.mockStore)
			}
			if tc.mockKVPublisher != nil {
				tc.mockKVPublisher(mtester.mockStatusValueKV)
			}

			recorder := httptest.NewRecorder()

			server.ServeHTTP(recorder, tc.request(t))
			tc.assertResponse(t, recorder)
		})
	}
}

func TestTaskQuery(t *testing.T) {
	serverID := uuid.New()
	conditionID := uuid.New()
	conditionKind := rctypes.FirmwareInstall

	mtester, server, err := setupTestServer(t)
	if err != nil {
		t.Fatal(err)
	}

	surl := fmt.Sprintf("/api/v1/servers/%s/condition-task/%s", serverID, conditionKind)

	testcases := []struct {
		name           string
		mockRepository func(r *store.MockRepository)
		mockTaskKV     func(tk *MocktaskKV)
		request        func(t *testing.T) *http.Request
		assertResponse func(t *testing.T, r *httptest.ResponseRecorder)
	}{
		{
			name: "invalid server id",
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, fmt.Sprintf("/api/v1/servers/%s/condition-task/%s", "invalid_serverid", conditionKind), http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "invalid server id")
			},
		},
		{
			name: "unsupported condition kind",
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%s/condition-task/invalidkind", serverID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, url, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "unsupported condition kind")
			},
		},
		{
			name: "repository error",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(nil, errors.Wrap(store.ErrRepository, "cosmic rays")).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, surl, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "condition lookup")
			},
		},
		{
			name: "no active/pending condition found",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(nil, store.ErrConditionNotFound).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, surl, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusNotFound, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), store.ErrConditionNotFound.Error())
			},
		},
		{
			name: "task KV query error",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: conditionID, Kind: conditionKind, State: rctypes.Active},
						nil,
					).
					Once()
			},
			mockTaskKV: func(tk *MocktaskKV) {
				tk.On("get", mock.Anything, conditionKind, conditionID, serverID).
					Return(nil, errQueryTask).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				fmt.Println(surl)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, surl, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), errQueryTask.Error())
			},
		},
		{
			name: "task obj stale/mismatch",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: conditionID, Kind: conditionKind, State: rctypes.Active},
						nil,
					).
					Once()
			},
			mockTaskKV: func(tk *MocktaskKV) {
				tk.On("get", mock.Anything, conditionKind, conditionID, serverID).
					Return(nil, errStaleTask).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, surl, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusUnprocessableEntity, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), errStaleTask.Error())
			},
		},
		{
			name: "successful task query",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(
						&rctypes.Condition{ID: conditionID, Kind: conditionKind, State: rctypes.Active},
						nil,
					).
					Once()
			},
			mockTaskKV: func(tk *MocktaskKV) {
				tk.On("get", mock.Anything, conditionKind, conditionID, serverID).
					Return(&rctypes.Task[any, any]{ID: conditionID, Kind: conditionKind}, nil).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, surl, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
				var response v1types.ServerResponse
				err := json.Unmarshal(asBytes(t, r.Body), &response)
				assert.NoError(t, err)
				assert.Equal(t, "Task identified", response.Message)
				assert.NotNil(t, response.Task)
				assert.Equal(t, conditionID, response.Task.ID)
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mockRepository != nil {
				tc.mockRepository(mtester.mockStore)
			}
			if tc.mockTaskKV != nil {
				tc.mockTaskKV(mtester.mocktaskKV)
			}

			recorder := httptest.NewRecorder()

			server.ServeHTTP(recorder, tc.request(t))
			tc.assertResponse(t, recorder)
		})
	}
}

func TestTaskPublish(t *testing.T) {
	serverID := uuid.New()
	conditionID := uuid.New()
	conditionKind := rctypes.FirmwareInstall

	mtester, server, err := setupTestServer(t)
	if err != nil {
		t.Fatal(err)
	}

	surl := fmt.Sprintf("/api/v1/servers/%s/condition-task/%s/%s", serverID, conditionKind, conditionID)

	testcases := []struct {
		name           string
		mockStore      func(r *store.MockRepository)
		mockTaskKV     func(tk *MocktaskKV)
		request        func(t *testing.T) *http.Request
		assertResponse func(t *testing.T, r *httptest.ResponseRecorder)
	}{
		{
			name: "invalid condition kind",
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%s/condition-task/invalidkind/%s", serverID, conditionID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewBufferString("{}"))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "unsupported condition kind")
			},
		},
		{
			name: "no active condition",
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(nil, errors.Wrap(store.ErrConditionNotFound, "expected an active condition")).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				task := rctypes.Task[any, any]{ID: conditionID, Kind: conditionKind}
				payload, _ := json.Marshal(task)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, surl, bytes.NewBuffer(payload))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusNotFound, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "expected an active condition: condition not found")
			},
		},
		{
			name: "task ID mismatch",
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(&rctypes.Condition{ID: uuid.New(), Kind: conditionKind}, nil).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				task := rctypes.Task[any, any]{ID: conditionID, Kind: conditionKind}
				payload, _ := json.Marshal(task)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, surl, bytes.NewBuffer(payload))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "does not match active ConditionID")
			},
		},
		{
			name: "successful task publish",
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(&rctypes.Condition{ID: conditionID, Kind: conditionKind}, nil).
					Once()
			},
			mockTaskKV: func(tk *MocktaskKV) {
				tk.On("publish", mock.Anything, serverID.String(), conditionID.String(), conditionKind, mock.IsType(&rctypes.Task[any, any]{}), false).
					Return(nil).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				task := rctypes.Task[any, any]{ID: conditionID, Kind: conditionKind}
				payload, _ := json.Marshal(task)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, surl, bytes.NewBuffer(payload))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "condition Task published")
			},
		},
		{
			name: "timestamp update only",
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(&rctypes.Condition{ID: conditionID, Kind: conditionKind}, nil).
					Once()
			},
			mockTaskKV: func(tk *MocktaskKV) {
				tk.On("publish", mock.Anything, serverID.String(), conditionID.String(), conditionKind, mock.IsType(&rctypes.Task[any, any]{ID: conditionID}), true).
					Return(nil).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("%s?ts_update=true", surl)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "condition Task published")
			},
		},
		{
			name: "task KV publish error",
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(&rctypes.Condition{ID: conditionID, Kind: conditionKind}, nil).
					Once()
			},
			mockTaskKV: func(tk *MocktaskKV) {
				tk.On("publish", mock.Anything, serverID.String(), conditionID.String(), conditionKind, mock.IsType(&rctypes.Task[any, any]{}), false).
					Return(fmt.Errorf("task KV publish error")).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				task := rctypes.Task[any, any]{ID: conditionID, Kind: conditionKind}
				payload, _ := json.Marshal(task)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, surl, bytes.NewBuffer(payload))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", "application/json")
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "Task publish error")
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mockStore != nil {
				tc.mockStore(mtester.mockStore)
			}
			if tc.mockTaskKV != nil {
				tc.mockTaskKV(mtester.mocktaskKV)
			}

			recorder := httptest.NewRecorder()

			server.ServeHTTP(recorder, tc.request(t))
			tc.assertResponse(t, recorder)
		})
	}
}

func TestConditionGet(t *testing.T) {
	serverID := uuid.New()
	conditionKind := rctypes.FirmwareInstall
	conditionID := uuid.New()

	mtester, server, err := setupTestServer(t)
	if err != nil {
		t.Fatal(err)
	}

	surl := fmt.Sprintf("/api/v1/servers/%s/condition", serverID)

	testcases := []struct {
		name           string
		mockRepository func(r *store.MockRepository)
		request        func(t *testing.T) *http.Request
		assertResponse func(t *testing.T, r *httptest.ResponseRecorder)
	}{
		{
			name: "invalid server id",
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, "/api/v1/servers/invalid-uuid/condition", nil)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "invalid server id")
			},
		},
		{
			name: "no pending/active condition found",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(nil, store.ErrConditionNotFound).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, surl, nil)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusNotFound, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "no pending/active condition not found for server")
			},
		},
		{
			name: "repository error",
			mockRepository: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(nil, errors.New("repository error")).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, surl, nil)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "condition lookup")
			},
		},
		{
			name: "active condition found",
			mockRepository: func(r *store.MockRepository) {
				condition := &rctypes.Condition{
					ID:    conditionID,
					Kind:  conditionKind,
					State: rctypes.Active,
				}
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(condition, nil).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, surl, nil)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
				var response v1types.ServerResponse
				err := json.Unmarshal(asBytes(t, r.Body), &response)
				assert.NoError(t, err)
				assert.NotNil(t, response.Condition)
				assert.Equal(t, conditionID, response.Condition.ID)
				assert.Equal(t, conditionKind, response.Condition.Kind)
				assert.Equal(t, rctypes.Active, response.Condition.State)
				assert.Contains(t, response.Message, "found condition in state: active")
			},
		},
		{
			name: "pending condition found",
			mockRepository: func(r *store.MockRepository) {
				condition := &rctypes.Condition{
					ID:    conditionID,
					Kind:  conditionKind,
					State: rctypes.Pending,
				}
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(condition, nil).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, surl, nil)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
				var response v1types.ServerResponse
				err := json.Unmarshal(asBytes(t, r.Body), &response)
				assert.NoError(t, err)
				assert.NotNil(t, response.Condition)
				assert.Equal(t, conditionID, response.Condition.ID)
				assert.Equal(t, conditionKind, response.Condition.Kind)
				assert.Equal(t, rctypes.Pending, response.Condition.State)
				assert.Contains(t, response.Message, "found condition in state: pending")
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mockRepository != nil {
				tc.mockRepository(mtester.mockStore)
			}

			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, tc.request(t))
			tc.assertResponse(t, recorder)
		})
	}
}
