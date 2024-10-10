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
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/fleetdb"
	"github.com/metal-automata/conditionorc/internal/model"
	"github.com/metal-automata/conditionorc/internal/store"
	"github.com/metal-automata/conditionorc/pkg/api/v1/conditions/types"
	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/conditions/types"
	fleetdbapi "github.com/metal-automata/fleetdb/pkg/api/v1"
	rctypes "github.com/metal-automata/rivets/condition"
	"github.com/metal-automata/rivets/events"
	eventsm "github.com/metal-automata/rivets/events"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func mockserver(t *testing.T, logger *logrus.Logger, fleetDBClient fleetdb.FleetDB, repository store.Repository, stream events.Stream) (*gin.Engine, error) {
	t.Helper()

	gin.SetMode(gin.ReleaseMode)
	g := gin.New()
	g.Use(gin.Recovery())

	options := []Option{
		WithLogger(logger),
		WithStore(repository),
		WithFleetDBClient(fleetDBClient),
		WithConditionDefinitions(
			[]*rctypes.Definition{
				{Kind: rctypes.FirmwareInstall},
			},
		),
	}

	if stream != nil {
		options = append(options, WithStreamBroker(stream, "foo"))
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

func asJSONBytes(t *testing.T, s *v1types.ServerResponse) []byte {
	t.Helper()

	b, err := json.Marshal(s)
	if err != nil {
		t.Error(err)
	}

	return b
}

func setupTestServer(t *testing.T) (*store.MockRepository, *fleetdb.MockFleetDB, *eventsm.MockStream, *gin.Engine, error) {
	repository := store.NewMockRepository(t)
	fleetDBClient := fleetdb.NewMockFleetDB(t)
	stream := eventsm.NewMockStream(t)

	server, err := mockserver(t, logrus.New(), fleetDBClient, repository, stream)

	return repository, fleetDBClient, stream, server, err
}

// nolint:gocyclo // cyclomatic tests are cyclomatic
func TestAddServer(t *testing.T) {
	repository, fleetDBClient, stream, server, err := setupTestServer(t)
	if err != nil {
		t.Fatal(err)
	}

	mockServerID := uuid.New()
	mockFacilityCode := "mock-facility-code"
	mockIP := "mock-ip"
	mockUser := "mock-user"
	mockPwd := "mock-pwd"
	validParams := types.AddServerParams{
		Facility: "mock-facility-code",
		IP:       "mock-ip",
		Username: "mock-user",
		Password: "mock-pwd",
	}
	// collect_bios_cfg is default to false since we don't set it in validParams.
	expectedInventoryParams := func(id string) string {
		return fmt.Sprintf(`{"collect_bios_cfg":true,"collect_firmware_status":true,"inventory_method":"outofband","asset_id":"%v"}`, id)
	}
	nopRollback := func() error {
		return nil
	}
	var generatedServerID uuid.UUID
	testcases := []struct {
		name              string
		mockStore         func(r *store.MockRepository)
		mockFleetDBClient func(f *fleetdb.MockFleetDB)
		mockStream        func(r *eventsm.MockStream)
		request           func(t *testing.T) *http.Request
		assertResponse    func(t *testing.T, r *httptest.ResponseRecorder)
	}{
		{
			name: "add server success",
			// mock repository
			mockStore: func(r *store.MockRepository) {
				// create condition query
				r.On("Create", mock.Anything, mockServerID, mockFacilityCode, mock.Anything).
					Return(nil).
					Run(func(args mock.Arguments) {
						c := args.Get(3).(*rctypes.Condition)
						assert.Equal(t, rctypes.ConditionStructVersion, c.Version, "condition version mismatch")
						assert.Equal(t, rctypes.Inventory, c.Kind, "condition kind mismatch")
						assert.Equal(t, json.RawMessage(expectedInventoryParams(mockServerID.String())), c.Parameters, "condition parameters mismatch")
						assert.Equal(t, rctypes.Pending, c.State, "condition state mismatch")
					}).
					Once()
			},
			mockFleetDBClient: func(f *fleetdb.MockFleetDB) {
				// lookup for an existing condition
				f.On("AddServer", mock.Anything, mockServerID, mockFacilityCode, mockIP, mockUser, mockPwd).
					Return(nopRollback, nil). // no condition exists
					Once()
			},
			mockStream: func(r *eventsm.MockStream) {
				r.On(
					"Publish",
					mock.Anything,
					fmt.Sprintf("%s.servers.%s", mockFacilityCode, rctypes.Inventory),
					mock.Anything,
				).Return(nil).Once()
			},
			request: func(t *testing.T) *http.Request {
				payload, err := json.Marshal(&v1types.ConditionCreate{Parameters: validParams.MustJSON()})
				if err != nil {
					t.Error()
				}

				url := fmt.Sprintf("/api/v1/serverEnroll/%v", mockServerID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
			},
		},
		{
			name:              "add server invalid params",
			mockStore:         nil,
			mockFleetDBClient: nil,
			mockStream:        nil,
			request: func(t *testing.T) *http.Request {
				payload := []byte("invalid json")
				url := fmt.Sprintf("/api/v1/serverEnroll/%v", mockServerID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "invalid ConditionCreate payload")
			},
		},
		{
			name:      "no bmc user",
			mockStore: nil,
			mockFleetDBClient: func(f *fleetdb.MockFleetDB) {
				// lookup for an existing condition
				f.On("AddServer", mock.Anything, mockServerID, mockFacilityCode, mockIP, "", mockPwd).
					Return(nopRollback, fleetdb.ErrBMCCredentials). // no condition exists
					Once()
			},
			mockStream: nil,
			request: func(t *testing.T) *http.Request {
				noUserParams := types.AddServerParams{
					Facility: "mock-facility-code",
					IP:       "mock-ip",
					Password: "mock-pwd",
				}
				payload, err := json.Marshal(&v1types.ConditionCreate{Parameters: noUserParams.MustJSON()})
				if err != nil {
					t.Error()
				}

				url := fmt.Sprintf("/api/v1/serverEnroll/%v", mockServerID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), fleetdb.ErrBMCCredentials.Error())
			},
		},
		{
			name:      "no bmc password",
			mockStore: nil,
			mockFleetDBClient: func(f *fleetdb.MockFleetDB) {
				// lookup for an existing condition
				f.On("AddServer", mock.Anything, mockServerID, mockFacilityCode, mockIP, mockUser, "").
					Return(nopRollback, fleetdb.ErrBMCCredentials). // no condition exists
					Once()
			},
			mockStream: nil,
			request: func(t *testing.T) *http.Request {
				noPwdParams := types.AddServerParams{
					Facility: "mock-facility-code",
					IP:       "mock-ip",
					Username: "mock-user",
				}
				payload, err := json.Marshal(&v1types.ConditionCreate{Parameters: noPwdParams.MustJSON()})
				if err != nil {
					t.Error()
				}

				url := fmt.Sprintf("/api/v1/serverEnroll/%v", mockServerID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), fleetdb.ErrBMCCredentials.Error())
			},
		},
		{
			name: "add server success no uuid param",
			// mock repository
			mockStore: func(r *store.MockRepository) {
				// create condition query
				r.On("Create", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(nil).
					Run(func(args mock.Arguments) {
						id := args.Get(1).(uuid.UUID)
						fc := args.Get(2).(string)
						c := args.Get(3).(*rctypes.Condition)
						assert.Equal(t, generatedServerID, id, "server ID mismatch")
						assert.Equal(t, mockFacilityCode, fc, "facility mismatch")
						assert.Equal(t, json.RawMessage(expectedInventoryParams(generatedServerID.String())), c.Parameters, "condition parameters mismatch")
						assert.Equal(t, rctypes.ConditionStructVersion, c.Version, "condition version mismatch")
						assert.Equal(t, rctypes.Inventory, c.Kind, "condition kind mismatch")
						assert.Equal(t, rctypes.Pending, c.State, "condition state mismatch")
					}).
					Once()
			},
			mockFleetDBClient: func(f *fleetdb.MockFleetDB) {
				// lookup for an existing condition
				f.On("AddServer", mock.Anything, mock.Anything, mockFacilityCode, mockIP, mockUser, mockPwd).
					Return(func(ctx context.Context, serverID uuid.UUID, _, _, _, _ string) (func() error, error) {
						generatedServerID = serverID
						return nopRollback, nil
					}).
					Once()
			},
			mockStream: func(r *eventsm.MockStream) {
				r.On(
					"Publish",
					mock.Anything,
					fmt.Sprintf("%s.servers.%s", mockFacilityCode, rctypes.Inventory),
					mock.Anything,
				).Return(nil).Once()
			},
			request: func(t *testing.T) *http.Request {
				payload, err := json.Marshal(&v1types.ConditionCreate{Parameters: validParams.MustJSON()})
				if err != nil {
					t.Error()
				}

				url := "/api/v1/serverEnroll/"
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mockStore != nil {
				tc.mockStore(repository)
			}

			if tc.mockFleetDBClient != nil {
				tc.mockFleetDBClient(fleetDBClient)
			}

			if tc.mockStream != nil {
				tc.mockStream(stream)
			}

			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, tc.request(t))

			tc.assertResponse(t, recorder)
		})
	}
}

// nolint:gocyclo // cyclomatic tests are cyclomatic
func TestDeleteServer(t *testing.T) {
	repository, fleetDBClient, _, server, err := setupTestServer(t)
	if err != nil {
		t.Fatal(err)
	}

	mockServerID := uuid.New()
	testcases := []struct {
		name              string
		mockStore         func(r *store.MockRepository)
		mockFleetDBClient func(f *fleetdb.MockFleetDB)
		request           func(t *testing.T) *http.Request
		assertResponse    func(t *testing.T, r *httptest.ResponseRecorder)
	}{
		{
			name: "delete server success",
			// mock repository
			mockStore: func(r *store.MockRepository) {
				// create condition query
				r.On("GetActiveCondition", mock.Anything, mockServerID).
					Return(nil, nil).
					Once()
			},
			mockFleetDBClient: func(r *fleetdb.MockFleetDB) {
				// lookup for an existing condition
				r.On("DeleteServer", mock.Anything, mockServerID).
					Return(nil).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%v", mockServerID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodDelete, url, nil)
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
			},
		},
		{
			name: "invalid ID",
			// mock repository
			mockStore:         func(r *store.MockRepository) {},
			mockFleetDBClient: func(r *fleetdb.MockFleetDB) {},
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%v", "invalidID")
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodDelete, url, nil)
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
			},
		},
		{
			name: "active condition",
			// mock repository
			mockStore: func(r *store.MockRepository) {
				// create condition query
				r.On("GetActiveCondition", mock.Anything, mockServerID).
					Return(&rctypes.Condition{}, nil).
					Once()
			},
			mockFleetDBClient: func(r *fleetdb.MockFleetDB) {},
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%v", mockServerID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodDelete, url, nil)
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
			},
		},
		{
			name: "check active condition error",
			// mock repository
			mockStore: func(r *store.MockRepository) {
				// create condition query
				r.On("GetActiveCondition", mock.Anything, mockServerID).
					Return(nil, fmt.Errorf("fake check condition error")).
					Once()
			},
			mockFleetDBClient: func(r *fleetdb.MockFleetDB) {},
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%v", mockServerID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodDelete, url, nil)
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusServiceUnavailable, r.Code)
			},
		},
		{
			name: "delete error",
			// mock repository
			mockStore: func(r *store.MockRepository) {
				// create condition query
				r.On("GetActiveCondition", mock.Anything, mockServerID).
					Return(nil, nil).
					Once()
			},
			mockFleetDBClient: func(r *fleetdb.MockFleetDB) {
				// lookup for an existing condition
				r.On("DeleteServer", mock.Anything, mockServerID).
					Return(fmt.Errorf("fake delete error")).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%v", mockServerID)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodDelete, url, nil)
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mockStore != nil {
				tc.mockStore(repository)
			}

			if tc.mockFleetDBClient != nil {
				tc.mockFleetDBClient(fleetDBClient)
			}

			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, tc.request(t))

			tc.assertResponse(t, recorder)
		})
	}
}

// nolint:gocyclo // cyclomatic tests are cyclomatic
func TestAddServerRollback(t *testing.T) {
	repository, fleetDBClient, stream, server, err := setupTestServer(t)
	if err != nil {
		t.Fatal(err)
	}
	rollbackCallCounter := 0
	rollback := func() error {
		rollbackCallCounter += 1
		return nil
	}
	mockServerID := uuid.New()
	validParams := fmt.Sprintf(`{"facility":"%v","ip":"192.168.0.1","user":"foo","pwd":"bar"}`, mockServerID)
	payload, err := json.Marshal(&v1types.ConditionCreate{Parameters: []byte(validParams)})
	if err != nil {
		t.Error()
	}
	requestFunc := func(t *testing.T) *http.Request {
		url := fmt.Sprintf("/api/v1/serverEnroll/%v", mockServerID)
		request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		return request
	}
	type mockError struct {
		calledTime int
		err        error
	}
	testcases := []struct {
		name                 string
		mockStoreCreateErr   mockError
		mockFleetDBClientErr mockError
		mockStreamErr        mockError
		mockStoreUpdateErr   mockError
		request              func(t *testing.T) *http.Request
		assertResponse       func(t *testing.T, r *httptest.ResponseRecorder)
		expectRollback       int
	}{
		{
			name:                 "no error",
			mockStoreCreateErr:   mockError{calledTime: 1, err: nil},
			mockFleetDBClientErr: mockError{calledTime: 1, err: nil},
			mockStreamErr:        mockError{calledTime: 1, err: nil},
			mockStoreUpdateErr:   mockError{calledTime: 0, err: nil},
			request:              requestFunc,
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
			},
			expectRollback: 0,
		},
		{
			name:                 "fleetdb error",
			mockStoreCreateErr:   mockError{calledTime: 0, err: nil},
			mockFleetDBClientErr: mockError{calledTime: 1, err: fmt.Errorf("fake fleetdb error")},
			mockStreamErr:        mockError{calledTime: 0, err: nil},
			mockStoreUpdateErr:   mockError{calledTime: 0, err: nil},
			request:              requestFunc,
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
			},
			expectRollback: 1,
		},
		{
			name:                 "repository create error",
			mockStoreCreateErr:   mockError{calledTime: 1, err: fmt.Errorf("fake repository create error")},
			mockFleetDBClientErr: mockError{calledTime: 1, err: nil},
			mockStreamErr:        mockError{calledTime: 0, err: nil},
			mockStoreUpdateErr:   mockError{calledTime: 0, err: nil},
			request:              requestFunc,
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
			},
			expectRollback: 1,
		},
		{
			name:                 "stream error",
			mockStoreCreateErr:   mockError{calledTime: 1, err: nil},
			mockFleetDBClientErr: mockError{calledTime: 1, err: nil},
			mockStreamErr:        mockError{calledTime: 1, err: fmt.Errorf("fake stream error")},
			mockStoreUpdateErr:   mockError{calledTime: 1, err: nil},
			request:              requestFunc,
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
			},
			expectRollback: 1,
		},
		{
			name:                 "stream delete error",
			mockStoreCreateErr:   mockError{calledTime: 1, err: nil},
			mockFleetDBClientErr: mockError{calledTime: 1, err: nil},
			mockStreamErr:        mockError{calledTime: 1, err: fmt.Errorf("fake stream error")},
			mockStoreUpdateErr:   mockError{calledTime: 1, err: fmt.Errorf("fake repository delete error")},
			request:              requestFunc,
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
			},
			expectRollback: 1,
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			rollbackCallCounter = 0
			if tc.mockStoreCreateErr.calledTime > 0 {
				repository.On("Create", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(tc.mockStoreCreateErr.err).
					Times(tc.mockStoreCreateErr.calledTime)
			}

			if tc.mockFleetDBClientErr.calledTime > 0 {
				fleetDBClient.On("AddServer", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(rollback, tc.mockFleetDBClientErr.err).
					Times(tc.mockFleetDBClientErr.calledTime)
			}

			if tc.mockStreamErr.calledTime > 0 {
				stream.On("Publish", mock.Anything, mock.Anything, mock.Anything).
					Return(tc.mockStreamErr.err).
					Times(tc.mockStreamErr.calledTime)
			}

			if tc.mockStoreUpdateErr.calledTime > 0 {
				repository.On("Update", mock.Anything, mock.Anything, mock.Anything).
					Return(tc.mockStoreUpdateErr.err).
					Times(tc.mockStoreUpdateErr.calledTime)
			}

			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, tc.request(t))
			tc.assertResponse(t, recorder)

			if rollbackCallCounter != tc.expectRollback {
				t.Errorf("rollback called %v times, expect %v", rollbackCallCounter, tc.expectRollback)
			}
		})
	}
}

// nolint:gocyclo // cyclomatic tests are cyclomatic
func TestServerConditionCreate(t *testing.T) {
	serverID := uuid.New()
	facilityCode := "foo-42"

	repository := store.NewMockRepository(t)
	fleetDBClient := fleetdb.NewMockFleetDB(t)
	stream := eventsm.NewMockStream(t)

	server, err := mockserver(t, logrus.New(), fleetDBClient, repository, stream)
	if err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name              string
		mockStore         func(r *store.MockRepository)
		mockFleetDBClient func(f *fleetdb.MockFleetDB)
		mockStream        func(r *eventsm.MockStream)
		request           func(t *testing.T) *http.Request
		assertResponse    func(t *testing.T, r *httptest.ResponseRecorder)
	}{
		{
			name:              "invalid server ID error",
			mockStore:         nil,
			mockFleetDBClient: nil,
			mockStream:        nil,
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%s/condition/%s", "123", "invalid")
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "invalid UUID")
			},
		},
		{
			name:              "invalid server condition state",
			mockStore:         nil,
			mockFleetDBClient: nil,
			mockStream:        nil,
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%s/condition/%s", uuid.New().String(), "asdasd")
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "unsupported condition")
			},
		},
		{
			name:              "invalid server condition payload returns error",
			mockStore:         nil,
			mockFleetDBClient: nil,
			mockStream:        nil,
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%s/condition/%s", serverID.String(), rctypes.FirmwareInstall)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader([]byte(``)))
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "invalid ConditionCreate payload")
			},
		},
		{
			name:      "server with no facility code returns error",
			mockStore: nil,
			mockFleetDBClient: func(m *fleetdb.MockFleetDB) {
				m.On("GetServer", mock.Anything, serverID).
					Return(&model.Server{ID: serverID, FacilityCode: ""}, nil).
					Once()
			},
			mockStream: nil,
			request: func(t *testing.T) *http.Request {
				payload, err := json.Marshal(&v1types.ConditionCreate{Parameters: []byte(`{"some param": "1"}`)})
				if err != nil {
					t.Error()
				}
				url := fmt.Sprintf("/api/v1/servers/%s/condition/%s", serverID.String(), rctypes.FirmwareInstall)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "no facility code")
			},
		},
		{
			name: "valid server condition created",
			mockStore: func(r *store.MockRepository) {
				parametersJSON, _ := json.Marshal(json.RawMessage(`{"some param": "1"}`))
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(nil, nil).
					Once()
				r.On("Create", mock.Anything, serverID, facilityCode, mock.IsType(&rctypes.Condition{})).
					Run(func(args mock.Arguments) {
						c := args.Get(3).(*rctypes.Condition)
						assert.Equal(t, rctypes.ConditionStructVersion, c.Version, "condition version mismatch")
						assert.Equal(t, rctypes.FirmwareInstall, c.Kind, "condition kind mismatch")
						assert.Equal(t, json.RawMessage(parametersJSON), c.Parameters, "condition parameters mismatch")
						assert.Equal(t, rctypes.Pending, c.State, "condition state mismatch")
					}).
					Return(nil).
					Once()
			},
			mockFleetDBClient: func(m *fleetdb.MockFleetDB) {
				m.On("GetServer", mock.Anything, serverID).
					Return(&model.Server{ID: serverID, FacilityCode: facilityCode}, nil).
					Once()
			},
			mockStream: func(r *eventsm.MockStream) {
				r.On(
					"Publish",
					mock.Anything,
					fmt.Sprintf("%s.servers.%s", facilityCode, rctypes.FirmwareInstall),
					mock.Anything,
				).Return(nil).Once()
			},
			request: func(t *testing.T) *http.Request {
				payload, err := json.Marshal(&v1types.ConditionCreate{Parameters: []byte(`{"some param": "1"}`)})
				if err != nil {
					t.Error()
				}
				url := fmt.Sprintf("/api/v1/servers/%s/condition/%s", serverID.String(), rctypes.FirmwareInstall)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
				var resp v1types.ServerResponse
				err := json.Unmarshal(r.Body.Bytes(), &resp)
				assert.NoError(t, err, "malformed response body")
				assert.Equal(t, "condition set", resp.Message)
				assert.Equal(t, 1, len(resp.Records.Conditions), "bad length of return conditions")
			},
		},
		{
			name: "condition with Fault created",
			mockStore: func(r *store.MockRepository) {
				get := r.On("GetActiveCondition", mock.Anything, serverID).
					Return(nil, nil).
					Once()

				create := r.On("Create", mock.Anything, serverID, facilityCode, mock.IsType(&rctypes.Condition{})).
					Run(func(args mock.Arguments) {
						c := args.Get(3).(*rctypes.Condition)
						expect := &rctypes.Fault{Panic: true, DelayDuration: "10s", FailAt: "foobar"}
						assert.Equal(t, c.Fault, expect)
					}).
					Return(nil).
					Times(1)

				create.NotBefore(get)
			},
			mockFleetDBClient: func(m *fleetdb.MockFleetDB) {
				m.On("GetServer", mock.Anything, serverID).
					Return(&model.Server{ID: serverID, FacilityCode: facilityCode}, nil).
					Once()
			},
			mockStream: func(r *eventsm.MockStream) {
				r.On(
					"Publish",
					mock.Anything,
					fmt.Sprintf("%s.servers.%s", facilityCode, rctypes.FirmwareInstall),
					mock.Anything,
				).Return(nil).Once()
			},
			request: func(t *testing.T) *http.Request {
				fault := rctypes.Fault{Panic: true, DelayDuration: "10s", FailAt: "foobar"}
				payload, err := json.Marshal(&v1types.ConditionCreate{Parameters: []byte(`{"some param": "1"}`), Fault: &fault})
				if err != nil {
					t.Error(err)
				}
				url := fmt.Sprintf("/api/v1/servers/%s/condition/%s", serverID.String(), rctypes.FirmwareInstall)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)
				var resp v1types.ServerResponse
				err := json.Unmarshal(r.Body.Bytes(), &resp)
				assert.NoError(t, err, "malformed response body")
				assert.Equal(t, "condition set", resp.Message)
				assert.Equal(t, 1, len(resp.Records.Conditions), "bad length of return conditions")
			},
		},
		{
			name: "server condition exists in non-finalized state",
			mockStore: func(r *store.MockRepository) {
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(&rctypes.Condition{
						Kind:       rctypes.FirmwareInstall,
						State:      rctypes.Pending,
						Parameters: []byte(`{"hello":"world"}`),
					}, nil).
					Once()
			},
			mockFleetDBClient: func(m *fleetdb.MockFleetDB) {
				m.On("GetServer", mock.Anything, serverID).
					Return(&model.Server{ID: serverID, FacilityCode: facilityCode}, nil).
					Once()
			},
			mockStream: nil,
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%s/condition/%s", serverID.String(), rctypes.FirmwareInstall)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader([]byte(`{"hello":"world"}`)))
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "server has an active condition")
			},
		},
		{
			name: "server condition publish failure results in created condition deletion",
			mockStore: func(r *store.MockRepository) {
				parametersJSON, _ := json.Marshal(json.RawMessage(`{"some param": "1"}`))
				r.On("GetActiveCondition", mock.Anything, serverID).
					Return(nil, nil).
					Once()
				r.On("Create", mock.Anything, serverID, facilityCode, mock.IsType(&rctypes.Condition{})).
					Run(func(args mock.Arguments) {
						c := args.Get(3).(*rctypes.Condition)
						assert.Equal(t, rctypes.ConditionStructVersion, c.Version, "condition version mismatch")
						assert.Equal(t, rctypes.FirmwareInstall, c.Kind, "condition kind mismatch")
						assert.Equal(t, json.RawMessage(parametersJSON), c.Parameters, "condition parameters mismatch")
						assert.Equal(t, rctypes.Pending, c.State, "condition state mismatch")
					}).
					Return(nil).
					Once()
				r.On("Update", mock.Anything, serverID, mock.IsType(&rctypes.Condition{})).
					Run(func(args mock.Arguments) {
						c := args.Get(2).(*rctypes.Condition)
						assert.Equal(t, rctypes.Failed, c.State)
					}).
					Return(nil).
					Once()
			},
			mockFleetDBClient: func(m *fleetdb.MockFleetDB) {
				m.On("GetServer", mock.Anything, serverID).
					Return(&model.Server{ID: serverID, FacilityCode: facilityCode}, nil).
					Once()
			},
			mockStream: func(r *eventsm.MockStream) {
				r.On(
					"Publish",
					mock.Anything,
					fmt.Sprintf("%s.servers.%s", facilityCode, rctypes.FirmwareInstall),
					mock.Anything,
				).
					Return(errors.New("gremlins in the pipes")).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				payload, err := json.Marshal(&v1types.ConditionCreate{Parameters: []byte(`{"some param": "1"}`)})
				if err != nil {
					t.Error()
				}
				url := fmt.Sprintf("/api/v1/servers/%s/condition/%s", serverID.String(), rctypes.FirmwareInstall)
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, url, bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusInternalServerError, r.Code)
				var resp v1types.ServerResponse
				err = json.Unmarshal(r.Body.Bytes(), &resp)
				assert.Nil(t, err)
				assert.Contains(t, resp.Message, "gremlins")
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mockStore != nil {
				tc.mockStore(repository)
			}

			if tc.mockStream != nil {
				tc.mockStream(stream)
			}

			if tc.mockFleetDBClient != nil {
				tc.mockFleetDBClient(fleetDBClient)
			}

			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, tc.request(t))

			tc.assertResponse(t, recorder)
		})
	}
}
func TestConditionStatus(t *testing.T) {
	serverID := uuid.New()
	condID := uuid.New()
	testCondition := &rctypes.Condition{
		ID:   condID,
		Kind: rctypes.FirmwareInstall,
	}
	conditionRecord := &store.ConditionRecord{
		ID:    condID,
		State: rctypes.Pending,
		Conditions: []*rctypes.Condition{
			testCondition,
		},
	}

	// mock repository
	repository := new(store.MockRepository)

	server, err := mockserver(t, logrus.New(), nil, repository, nil)
	if err != nil {
		t.Fatal(err)
	}

	testcases := []struct {
		name           string
		mockStore      func(r *store.MockRepository)
		request        func(t *testing.T) *http.Request
		assertResponse func(t *testing.T, r *httptest.ResponseRecorder)
	}{
		{
			name:      "invalid server ID error",
			mockStore: nil,
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%s/status", "123")
				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, url, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusBadRequest, r.Code)
				assert.Contains(t, string(asBytes(t, r.Body)), "invalid UUID")
			},
		},
		{
			name: "server condition record returned",
			mockStore: func(r *store.MockRepository) {
				r.On("Get", mock.Anything, serverID).
					Return(conditionRecord, nil).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%s/status", serverID.String())

				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, url, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusOK, r.Code)

				want := asJSONBytes(
					t,
					&v1types.ServerResponse{
						Records: &v1types.ConditionsResponse{
							ServerID: serverID,
							State:    rctypes.Pending,
							Conditions: []*rctypes.Condition{
								testCondition,
							},
						},
					},
				)

				assert.Equal(t, asBytes(t, r.Body), want)
			},
		},
		{
			name: "no server condition",
			mockStore: func(r *store.MockRepository) {
				r.On("Get", mock.Anything, serverID).
					Return(nil, store.ErrConditionNotFound).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%s/status", serverID.String())

				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, url, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusNotFound, r.Code)

				want := asJSONBytes(
					t,
					&v1types.ServerResponse{
						Message: "condition not found for server",
					},
				)

				assert.Equal(t, asBytes(t, r.Body), want)
			},
		},
		{
			name: "lookup error",
			mockStore: func(r *store.MockRepository) {
				r.On("Get", mock.Anything, serverID).
					Return(nil, errors.New("bogus error")).
					Once()
			},
			request: func(t *testing.T) *http.Request {
				url := fmt.Sprintf("/api/v1/servers/%s/status", serverID.String())

				request, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, url, http.NoBody)
				if err != nil {
					t.Fatal(err)
				}

				return request
			},
			assertResponse: func(t *testing.T, r *httptest.ResponseRecorder) {
				assert.Equal(t, http.StatusServiceUnavailable, r.Code)

				want := asJSONBytes(
					t,
					&v1types.ServerResponse{
						Message: "condition lookup: bogus error",
					},
				)

				assert.Equal(t, asBytes(t, r.Body), want)
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mockStore != nil {
				tc.mockStore(repository)
			}

			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, tc.request(t))

			tc.assertResponse(t, recorder)
		})
	}
}

func TestFirmwareInstallComposite(t *testing.T) {
	serverID := uuid.New()
	r := &Routes{}
	fwtp := rctypes.FirmwareInstallTaskParameters{
		AssetID:      serverID,
		ForceInstall: true,
	}

	boolPtr := func(b bool) *bool {
		return &b
	}
	testcases := []struct {
		name             string
		fwset            *fleetdbapi.ComponentFirmwareSet
		expectFirmwares  int
		expectConditions int
		expectInband     bool
	}{
		{
			"oob fw set",
			&fleetdbapi.ComponentFirmwareSet{
				Name: "foo",
				ComponentFirmware: []fleetdbapi.ComponentFirmwareVersion{
					{
						Vendor:        "bar",
						Filename:      "abc.bin",
						Version:       "1.1",
						RepositoryURL: "http://foo/abc.bin",
						Component:     "bmc",
						Checksum:      "foo",
						Model:         []string{"baz", "baz"},
					},
					{
						Vendor:        "foo",
						Filename:      "def.bin",
						Version:       "0.1",
						RepositoryURL: "http://bar/def.bin",
						Component:     "bios",
						Checksum:      "123",
						Model:         []string{"as", "ds"},
					},
				},
			},
			2,
			2,
			false,
		},
		{
			"inband fw set",
			&fleetdbapi.ComponentFirmwareSet{
				Name: "foo",
				ComponentFirmware: []fleetdbapi.ComponentFirmwareVersion{
					{
						Vendor:        "bar",
						Filename:      "abc.bin",
						Version:       "1.1",
						RepositoryURL: "http://foo/abc.bin",
						Component:     "bmc",
						Checksum:      "foo",
						Model:         []string{"baz", "baz"},
						InstallInband: boolPtr(true),
					},
					{
						Vendor:        "foo",
						Filename:      "def.bin",
						Version:       "0.1",
						RepositoryURL: "http://bar/def.bin",
						Component:     "bios",
						Checksum:      "123",
						Model:         []string{"as", "ds"},
					},
				},
			},
			2,
			4,
			true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.expectInband {
				os.Setenv("CONDITION_API_FEATURE_INBAND_FIRMWARE", "true")
			}

			got := r.firmwareInstallComposite(context.Background(), serverID, "foobar", fwtp, tc.fwset)
			assert.Equal(t, tc.expectConditions, len(got.Conditions))
			for _, cond := range got.Conditions {
				assert.NotEmpty(t, cond.Kind)
				assert.Equal(t, rctypes.Pending, cond.State)
				assert.Equal(t, rctypes.ConditionStructVersion, cond.Version)
				assert.NotZero(t, cond.CreatedAt)

				switch cond.Kind {
				case rctypes.FirmwareInstall, rctypes.FirmwareInstallInband:
					params := &rctypes.FirmwareInstallTaskParameters{}
					require.NoError(t, params.Unmarshal(cond.Parameters))
					assert.Equal(t, tc.expectFirmwares, len(params.Firmwares))
					assert.Equal(t, fwtp.ForceInstall, params.ForceInstall)
					assert.Equal(t, fwtp.AssetID, params.AssetID)
				case rctypes.Inventory:
					params := &rctypes.InventoryTaskParameters{}
					require.NoError(t, json.Unmarshal(cond.Parameters, &params))
					assert.Equal(t, fwtp.AssetID, params.AssetID)
				case rctypes.ServerControl:
					params := &rctypes.ServerControlTaskParameters{}
					require.NoError(t, json.Unmarshal(cond.Parameters, &params))
					assert.Equal(t, rctypes.PxeBootPersistent, params.Action)
					assert.True(t, params.SetNextBootDevicePersistent)
					assert.True(t, params.SetNextBootDeviceEFI)
				}
			}
		})
	}
}
