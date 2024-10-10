// nolint
package conditions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/fleetdb"
	"github.com/metal-automata/conditionorc/internal/model"
	"github.com/metal-automata/conditionorc/internal/server"
	fleetdbapi "github.com/metal-automata/fleetdb/pkg/api/v1"

	"github.com/metal-automata/conditionorc/internal/store"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/metal-automata/conditionorc/pkg/api/v1/conditions/types"
	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/conditions/types"
	rctypes "github.com/metal-automata/rivets/condition"

	eventsm "github.com/metal-automata/rivets/events"
)

type integrationTester struct {
	t               *testing.T
	assertAuthToken bool
	handler         http.Handler
	client          *Client
	repository      *store.MockRepository
	fleetDB         *fleetdb.MockFleetDB
	stream          *eventsm.MockStream
}

// Do implements the HTTPRequestDoer interface to swap the response writer
func (i *integrationTester) Do(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}

	w := httptest.NewRecorder()
	i.handler.ServeHTTP(w, req)

	if i.assertAuthToken {
		require.NotEmpty(i.t, req.Header.Get("Authorization"))
	} else {
		require.Empty(i.t, req.Header.Get("Authorization"))
	}

	return w.Result(), nil
}

func newTester(t *testing.T) *integrationTester {
	t.Helper()

	repository := store.NewMockRepository(t)
	fleetDBClient := fleetdb.NewMockFleetDB(t)
	stream := eventsm.NewMockStream(t)

	l := logrus.New()
	l.Level = logrus.Level(logrus.ErrorLevel)
	serverOptions := []server.Option{
		server.WithLogger(l),
		server.WithListenAddress("localhost:9999"),
		server.WithStore(repository),
		server.WithFleetDBClient(fleetDBClient),
		server.WithStreamBroker(stream, "foo"),
		server.WithConditionDefinitions(
			[]*rctypes.Definition{
				{Kind: rctypes.FirmwareInstall},
			},
		),
	}

	gin.SetMode(gin.ReleaseMode)

	srv := server.New(serverOptions...)

	// setup test server httptest recorder
	tester := &integrationTester{
		t:          t,
		handler:    srv.Handler,
		repository: repository,
		fleetDB:    fleetDBClient,
		stream:     stream,
	}

	// setup test client
	clientOptions := []Option{WithHTTPClient(tester)}

	client, err := NewClient("http://localhost:9999", clientOptions...)
	if err != nil {
		t.Error(err)
	}

	tester.client = client
	return tester
}

func TestConditionStatus(t *testing.T) {
	serverID := uuid.New()

	testcases := []struct {
		name                string
		mockStore           func(r *store.MockRepository)
		expectResponse      func() *v1types.ServerResponse
		expectErrorContains string
	}{
		{
			name: "valid response",
			mockStore: func(r *store.MockRepository) {
				r.On(
					"Get",
					mock.Anything,
					serverID,
				).Return(
					&store.ConditionRecord{
						State: rctypes.Pending,
						Conditions: []*rctypes.Condition{
							{
								Kind:   rctypes.FirmwareInstall,
								State:  rctypes.Pending,
								Status: []byte(`{"hello":"world"}`),
							},
						},
					},
					nil,
				).Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 200,
					Records: &v1types.ConditionsResponse{
						ServerID: serverID,
						State:    rctypes.Pending,
						Conditions: []*rctypes.Condition{
							{
								Kind:   rctypes.FirmwareInstall,
								State:  rctypes.Pending,
								Status: []byte(`{"hello":"world"}`),
							},
						},
					},
				}
			},
			expectErrorContains: "",
		},
		{
			name: "404 response",
			mockStore: func(r *store.MockRepository) {
				r.On(
					"Get",
					mock.Anything,
					serverID,
				).Return(
					nil,
					store.ErrConditionNotFound,
				).Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					Message:    "condition not found for server",
					StatusCode: 404,
				}
			},
			expectErrorContains: "",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tester := newTester(t)

			if tc.mockStore != nil {
				tc.mockStore(tester.repository)
			}

			got, err := tester.client.ServerConditionStatus(context.TODO(), serverID)
			if err != nil {
				t.Error(err)
			}

			if err != nil {
				require.Contains(t, err.Error(), tc.expectErrorContains)
			}

			if tc.expectErrorContains != "" && err == nil {
				t.Error("expected error, got nil")
			}

			require.Equal(
				t,
				tc.expectResponse(),
				got,
			)
		})
	}
}

func TestConditionCreate(t *testing.T) {
	serverID := uuid.New()

	testcases := []struct {
		name                string
		payload             v1types.ConditionCreate
		mockStore           func(r *store.MockRepository)
		expectResponse      func() *v1types.ServerResponse
		expectErrorContains string
		expectPublish       bool
	}{
		{
			name:    "valid payload sent",
			payload: v1types.ConditionCreate{Parameters: []byte(`{"hello":"world"}`)},
			mockStore: func(r *store.MockRepository) {
				r.On(
					"GetActiveCondition",
					mock.Anything,
					serverID,
				).Return(
					nil,
					nil,
				).Once()

				r.On(
					"Create",
					mock.Anything,
					serverID,
					mock.Anything,
					mock.Anything,
				).Return(
					func(_ context.Context, _ uuid.UUID, fc string, cs ...*rctypes.Condition) error {
						require.Equal(t, "facility", fc, "facility code mismatch")
						require.Len(t, cs, 1, "conditions length")
						c := cs[0]
						require.Equal(t, rctypes.ConditionStructVersion, c.Version, "condition version mismatch")
						require.Equal(t, rctypes.FirmwareInstall, c.Kind, "condition kind mismatch")
						require.Equal(t, json.RawMessage(`{"hello":"world"}`), c.Parameters, "condition parameters mismatch")
						require.Equal(t, rctypes.Pending, c.State, "condition state mismatch")
						return nil
					},
				).Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 200,
					Message:    "condition set",
				}
			},
			expectErrorContains: "",
			expectPublish:       true,
		},
		{
			name:    "400 response",
			payload: v1types.ConditionCreate{Parameters: []byte(`{"hello":"world"}`)},
			mockStore: func(r *store.MockRepository) {
				r.On(
					"GetActiveCondition",
					mock.Anything,
					serverID,
				).Return(
					&rctypes.Condition{
						State: rctypes.Pending,
					},
					nil,
				).Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					Message:    "server has an active condition",
					StatusCode: 400,
				}
			},
			expectErrorContains: "",
			expectPublish:       false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tester := newTester(t)

			if tc.mockStore != nil {
				tc.mockStore(tester.repository)
			}

			tester.fleetDB.On("GetServer", mock.Anything, mock.Anything).Return(&model.Server{FacilityCode: "facility"}, nil).Once()

			if tc.expectPublish {
				tester.stream.On(
					"Publish",
					mock.Anything,
					mock.Anything,
					mock.Anything,
				).Return(nil).Once()
			}

			got, err := tester.client.ServerConditionCreate(context.TODO(), serverID, rctypes.FirmwareInstall, tc.payload)
			if err != nil {
				t.Fatal(err)
			}

			if err != nil {
				require.Contains(t, err.Error(), tc.expectErrorContains)
			}

			if tc.expectErrorContains != "" && err == nil {
				t.Error("expected error, got nil")
			}

			require.Equal(t, tc.expectResponse().StatusCode, got.StatusCode, "bad status code")
			require.Equal(t, tc.expectResponse().Message, got.Message, "bad message")
		})
	}
}

func TestFirmwareInstall(t *testing.T) {
	serverID := uuid.New()

	inband := true
	inbandfw := fleetdbapi.ComponentFirmwareVersion{
		Vendor:        "foobar",
		Model:         []string{"bar"},
		Version:       "0.0",
		InstallInband: &inband,
	}

	oobfw := fleetdbapi.ComponentFirmwareVersion{
		Vendor:  "foo",
		Model:   []string{"bar"},
		Version: "0.0",
	}

	fwset := &fleetdbapi.ComponentFirmwareSet{
		UUID:              uuid.New(),
		ComponentFirmware: []fleetdbapi.ComponentFirmwareVersion{},
	}

	testcases := []struct {
		name                string
		payload             *rctypes.FirmwareInstallTaskParameters
		mockStore           func(r *store.MockRepository)
		mockFleetDB         func(r *fleetdb.MockFleetDB)
		expectResponse      func() *v1types.ServerResponse
		expectErrorContains string
		expectPublish       bool
		inbandInstall       bool
	}{
		{
			name: "success case",
			payload: &rctypes.FirmwareInstallTaskParameters{
				AssetID:       serverID,
				FirmwareSetID: fwset.UUID,
			},
			mockStore: func(r *store.MockRepository) {
				r.On("Create", mock.Anything, serverID, "facility", mock.Anything, mock.Anything).
					Return(nil).Once()
			},
			mockFleetDB: func(r *fleetdb.MockFleetDB) {
				fwset.ComponentFirmware = append(fwset.ComponentFirmware, oobfw)
				r.On("FirmwareSetByID", mock.Anything, mock.Anything).
					Return(fwset, nil).Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 200,
					Message:    "firmware install scheduled",
				}
			},
			expectErrorContains: "",
			expectPublish:       true,
		},
		{
			name: "success case - inband install",
			payload: &rctypes.FirmwareInstallTaskParameters{
				AssetID:       serverID,
				FirmwareSetID: fwset.UUID,
			},
			mockStore: func(r *store.MockRepository) {
				r.On("Create", mock.Anything, serverID, "facility", mock.Anything, mock.Anything).
					Return(nil).Once()
			},
			mockFleetDB: func(r *fleetdb.MockFleetDB) {
				fwset.ComponentFirmware = append(fwset.ComponentFirmware, inbandfw, oobfw)
				r.On("FirmwareSetByID", mock.Anything, mock.Anything).
					Return(fwset, nil).Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 200,
					Message:    "firmware install scheduled",
				}
			},
			expectErrorContains: "",
			expectPublish:       true,
		},
		{
			name: "409 response",
			payload: &rctypes.FirmwareInstallTaskParameters{
				AssetID:       serverID,
				FirmwareSetID: fwset.UUID,
			},
			mockFleetDB: func(r *fleetdb.MockFleetDB) {
				fwset.ComponentFirmware = append(fwset.ComponentFirmware, oobfw)
				r.On("FirmwareSetByID", mock.Anything, mock.Anything).
					Return(fwset, nil).Once()
			},
			mockStore: func(r *store.MockRepository) {
				r.On("Create", mock.Anything, serverID, "facility", mock.Anything, mock.Anything).
					Return(fmt.Errorf("%w:%s", store.ErrActiveCondition, "pound sand")).Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					Message:    "server has an active condition",
					StatusCode: 409,
				}
			},
			expectErrorContains: "",
			expectPublish:       false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tester := newTester(t)

			if tc.mockStore != nil {
				tc.mockStore(tester.repository)
			}

			if tc.mockFleetDB != nil {
				tc.mockFleetDB(tester.fleetDB)
			}

			tester.fleetDB.On("GetServer", mock.Anything, mock.Anything).Return(&model.Server{FacilityCode: "facility"}, nil).Times(1)
			if tc.expectPublish {
				tester.stream.On(
					"Publish",
					mock.Anything,
					mock.Anything,
					mock.Anything,
				).Return(nil).Times(1)
			}

			got, err := tester.client.ServerFirmwareInstall(context.TODO(), tc.payload)
			if err != nil {
				t.Error(err)
			}

			if err != nil {
				require.Contains(t, err.Error(), tc.expectErrorContains)
			}

			if tc.expectErrorContains != "" && err == nil {
				t.Error("expected error, got nil")
			}

			require.Equal(t, tc.expectResponse().StatusCode, got.StatusCode, "bad status code")
			require.Contains(t, got.Message, tc.expectResponse().Message, "bad message")
		})
	}
}

func TestServerEnroll(t *testing.T) {
	serverID := uuid.New()
	validParams := types.AddServerParams{
		Facility: "mock-facility-code",
		IP:       "mock-ip",
		Username: "mock-user",
		Password: "mock-pwd",
	}
	invalidParamsNoBMCUser := types.AddServerParams{
		Facility: "mock-facility-code",
		IP:       "mock-ip",
		Password: "mock-pwd",
	}

	expectedInventoryParams := func(id string) string {
		return fmt.Sprintf(`{"collect_bios_cfg":true,"collect_firmware_status":true,"inventory_method":"outofband","asset_id":"%v"}`, id)
	}

	rollbackCounter := 0
	rollback := func() error {
		rollbackCounter += 1
		return nil
	}

	testcases := []struct {
		name                  string
		payload               v1types.ConditionCreate
		mockFleetDBClient     func(f *fleetdb.MockFleetDB)
		mockStore             func(r *store.MockRepository)
		expectedRollbackCount int
		expectResponse        func() *v1types.ServerResponse
		expectError           string
		expectPublish         bool
	}{
		{
			name:    "valid payload sent",
			payload: v1types.ConditionCreate{Parameters: validParams.MustJSON()},
			mockFleetDBClient: func(r *fleetdb.MockFleetDB) {
				// lookup for an existing condition
				r.On("AddServer",
					mock.Anything,
					serverID,
					"mock-facility-code",
					"mock-ip",
					"mock-user",
					"mock-pwd",
				).
					Return(nil, nil).
					Once()
			},
			mockStore: func(r *store.MockRepository) {
				// expect valid payload
				r.On("Create",
					mock.Anything,
					serverID,
					mock.Anything,
					mock.Anything,
				).Return(func(_ context.Context, _ uuid.UUID, _ string, cs ...*rctypes.Condition) error {
					require.Len(t, cs, 1)
					c := cs[0]
					require.Equal(t, rctypes.ConditionStructVersion, c.Version, "condition version mismatch")
					require.Equal(t, rctypes.Inventory, c.Kind, "condition kind mismatch")
					require.Equal(t, json.RawMessage(expectedInventoryParams(serverID.String())), c.Parameters, "condition parameters mismatch")
					require.Equal(t, rctypes.Pending, c.State, "condition state mismatch")
					return nil
				}).
					Once()
			},
			expectedRollbackCount: 0,
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 200,
					Message:    "condition set",
				}
			},
			expectError:   "",
			expectPublish: true,
		},
		{
			name:                  "invalid payload - failed to send out",
			payload:               v1types.ConditionCreate{Parameters: []byte("not json")},
			mockFleetDBClient:     func(r *fleetdb.MockFleetDB) {},
			mockStore:             func(r *store.MockRepository) {},
			expectedRollbackCount: 0,
			expectResponse: func() *v1types.ServerResponse {
				return nil
			},
			expectError:   "conditionorc client error - error in POST JSON payload",
			expectPublish: false,
		},
		{
			name:    "no bmc user",
			payload: v1types.ConditionCreate{Parameters: invalidParamsNoBMCUser.MustJSON()},
			mockFleetDBClient: func(r *fleetdb.MockFleetDB) {
				// lookup for an existing condition
				r.On("AddServer",
					mock.Anything,
					serverID,
					"mock-facility-code",
					"mock-ip",
					"",
					"mock-pwd",
				).
					Return(rollback, fleetdb.ErrBMCCredentials).
					Once()
			},
			mockStore:             func(r *store.MockRepository) {},
			expectedRollbackCount: 1,
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 500,
					Message:    "add server: invalid bmc credentials. missing user or passwordserver rollback err: <nil>",
				}
			},
			expectError:   "",
			expectPublish: false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tester := newTester(t)

			if tc.mockStore != nil {
				tc.mockStore(tester.repository)
			}

			if tc.mockFleetDBClient != nil {
				tc.mockFleetDBClient(tester.fleetDB)
			}

			if tc.expectPublish {
				tester.stream.On(
					"Publish",
					mock.Anything,
					mock.Anything,
					mock.Anything,
				).Return(nil).Times(1)
			}

			defer func() {
				require.Equal(t, tc.expectedRollbackCount, rollbackCounter, "rollback called incorrectly")
				rollbackCounter = 0
			}()

			got, err := tester.client.ServerEnroll(context.TODO(), serverID.String(), tc.payload)
			if tc.expectError != "" {
				require.Contains(t, err.Error(), tc.expectError, "request should not send out")
				if got != nil {
					t.Errorf("expect nil response, got %v", got)
				}
				return
			}

			if err != nil {
				t.Error(err)
			}

			if tc.expectResponse() != nil {
				require.Equal(t, tc.expectResponse().StatusCode, got.StatusCode, "bad status code")
				require.Equal(t, tc.expectResponse().Message, got.Message, "bad message")
			}
		})
	}
}

func TestServerEnrollEmptyUUID(t *testing.T) {
	tester := newTester(t)

	var generatedServerID uuid.UUID
	validParams := types.AddServerParams{
		Facility: "mock-facility-code",
		IP:       "mock-ip",
		Username: "mock-user",
		Password: "mock-pwd",
	}
	expectedInventoryParams := func(id string) string {
		return fmt.Sprintf(`{"collect_bios_cfg":true,"collect_firmware_status":true,"inventory_method":"outofband","asset_id":"%v"}`, id)
	}

	rollbackCounter := 0
	rollback := func() error {
		rollbackCounter += 1
		return nil
	}

	tester.fleetDB.On("AddServer",
		mock.Anything,
		mock.Anything,
		"mock-facility-code",
		"mock-ip",
		"mock-user",
		"mock-pwd").
		Return(rollback, nil).
		Once()

	tester.repository.On("Create",
		mock.Anything,
		mock.Anything,
		mock.Anything,
		mock.MatchedBy(func(c *rctypes.Condition) bool {
			generatedServerID = c.Target
			return c.Version == rctypes.ConditionStructVersion &&
				c.Kind == rctypes.Inventory &&
				bytes.Equal(c.Parameters, []byte(expectedInventoryParams(generatedServerID.String()))) &&
				c.State == rctypes.Pending
		})).
		Return(nil).
		Once()

	tester.stream.On("Publish",
		mock.Anything,
		mock.Anything,
		mock.Anything,
	).Return(nil)

	got, err := tester.client.ServerEnroll(context.TODO(), "", v1types.ConditionCreate{Parameters: validParams.MustJSON()})
	if err != nil {
		t.Error(err)
	}
	require.Equal(t, http.StatusOK, got.StatusCode, "bad status code")
	require.Equal(t, 0, rollbackCounter, "rollback called incorrectly")
}

func TestServerDelete(t *testing.T) {
	serverID := uuid.New()
	testcases := []struct {
		name              string
		mockFleetDBClient func(f *fleetdb.MockFleetDB)
		mockStore         func(r *store.MockRepository)
		expectResponse    func() *v1types.ServerResponse
	}{
		{
			"delete server success",
			func(r *fleetdb.MockFleetDB) {
				r.On("DeleteServer",
					mock.Anything,
					serverID,
				).Return(nil).Once()
			},
			func(r *store.MockRepository) {
				// expect valid payload
				r.On("GetActiveCondition",
					mock.Anything,
					serverID,
				).Return(nil, nil).Once()
			},
			func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 200,
					Message:    "server deleted",
				}
			},
		},
		{
			"active condition",
			func(r *fleetdb.MockFleetDB) {},
			func(r *store.MockRepository) {
				// expect valid payload
				r.On("GetActiveCondition",
					mock.Anything,
					serverID,
				).Return(&rctypes.Condition{}, nil).Once()
			},
			func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 400,
					Message:    "failed to delete server because it has an active condition",
				}
			},
		},
		{
			"check active condition error",
			func(r *fleetdb.MockFleetDB) {},
			func(r *store.MockRepository) {
				// expect valid payload
				r.On("GetActiveCondition",
					mock.Anything,
					serverID,
				).Return(nil, fmt.Errorf("fake check condition error")).Once()
			},
			func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 503,
					Message:    "error checking server state: fake check condition error",
				}
			},
		},
		{
			"fleetdb delete error",
			func(r *fleetdb.MockFleetDB) {
				r.On("DeleteServer",
					mock.Anything,
					mock.Anything,
				).Return(fmt.Errorf("fake delete error")).Once()
			},
			func(r *store.MockRepository) {
				// expect valid payload
				r.On("GetActiveCondition",
					mock.Anything,
					serverID,
				).Return(nil, nil).Once()
			},
			func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 500,
					Message:    "fake delete error",
				}
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tester := newTester(t)

			if tc.mockStore != nil {
				tc.mockStore(tester.repository)
			}

			if tc.mockFleetDBClient != nil {
				tc.mockFleetDBClient(tester.fleetDB)
			}

			got, err := tester.client.ServerDelete(context.TODO(), serverID.String())
			if err != nil {
				t.Error(err)
			}
			require.Equal(t, tc.expectResponse().StatusCode, got.StatusCode, "bad status code")
			require.Equal(t, tc.expectResponse().Message, got.Message, "bad message")
		})
	}
}

func TestServerDeleteInvalidUUID(t *testing.T) {
	tester := newTester(t)

	fakeInvalidServerID := "fakeInvalidID"

	testcases := []struct {
		name           string
		serverID       string
		expectResponse func() *v1types.ServerResponse
	}{
		{
			"empty ID",
			"",
			func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 404,
					Message:    "invalid request - route not found",
				}
			},
		},
		{
			"invalid ID",
			fakeInvalidServerID,
			func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 400,
					Message:    fmt.Sprintf("invalid UUID length: %d", len(fakeInvalidServerID)),
				}
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tester.client.ServerDelete(context.TODO(), tc.serverID)
			if err != nil {
				t.Error(err)
			}
			require.Equal(t, tc.expectResponse().StatusCode, got.StatusCode, "bad status code")
			require.Equal(t, tc.expectResponse().Message, got.Message, "bad message")
		})
	}
}

func TestServerBiosControl(t *testing.T) {
	serverID := uuid.New()

	validParams := rctypes.BiosControlTaskParameters{
		AssetID: serverID,
		Action:  rctypes.ResetConfig,
	}

	testcases := []struct {
		name                string
		payload             *rctypes.BiosControlTaskParameters
		mockStore           func(r *store.MockRepository)
		mockFleetDB         func(r *fleetdb.MockFleetDB)
		expectResponse      func() *v1types.ServerResponse
		expectErrorContains string
		expectPublish       bool
	}{
		{
			name:    "success case",
			payload: &validParams,
			mockStore: func(r *store.MockRepository) {
				r.On("Create", mock.Anything, serverID, "facility", mock.Anything, mock.Anything).
					Return(nil).Once()
			},
			mockFleetDB: func(r *fleetdb.MockFleetDB) {
				r.On("GetServer", mock.Anything, mock.Anything).
					Return(&model.Server{FacilityCode: "facility"}, nil).Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 200,
					Message:    "condition set",
				}
			},
			expectErrorContains: "",
			expectPublish:       true,
		},
		{
			name:    "no server",
			payload: &validParams,
			mockFleetDB: func(r *fleetdb.MockFleetDB) {
				r.On("GetServer", mock.Anything, mock.Anything).
					Return(nil, fmt.Errorf("no server")).Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 500,
					Message:    "server facility: no server",
				}
			},
			expectErrorContains: "",
			expectPublish:       false,
		},
		{
			name:    "active condition error",
			payload: &validParams,
			mockStore: func(r *store.MockRepository) {
				r.On("Create", mock.Anything, serverID, "facility", mock.Anything, mock.Anything).
					Return(fmt.Errorf("%w:%s", store.ErrActiveCondition, "pound sand")).Once()
			},
			mockFleetDB: func(r *fleetdb.MockFleetDB) {
				r.On("GetServer", mock.Anything, mock.Anything).
					Return(&model.Server{FacilityCode: "facility"}, nil).Once()
			},
			expectResponse: func() *v1types.ServerResponse {
				return &v1types.ServerResponse{
					StatusCode: 500,
					Message:    "server has an active condition",
				}
			},
			expectErrorContains: "",
			expectPublish:       false,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			tester := newTester(t)

			if tc.mockStore != nil {
				tc.mockStore(tester.repository)
			}

			if tc.mockFleetDB != nil {
				tc.mockFleetDB(tester.fleetDB)
			}

			if tc.expectPublish {
				tester.stream.On(
					"Publish",
					mock.Anything,
					mock.Anything,
					mock.Anything,
				).Return(nil).Times(1)
			}

			got, err := tester.client.ServerBiosControl(context.TODO(), tc.payload)
			if err != nil {
				t.Error(err)
			}

			if err != nil {
				require.Contains(t, err.Error(), tc.expectErrorContains)
			}

			if tc.expectErrorContains != "" && err == nil {
				t.Error("expected error, got nil")
			}

			require.Equal(t, tc.expectResponse().StatusCode, got.StatusCode, "bad status code")
			require.Contains(t, got.Message, tc.expectResponse().Message, "bad message")
		})
	}
}
