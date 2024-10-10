package routes

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/model"
	"github.com/metal-automata/conditionorc/internal/store"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	fwValidationUrl string = "/api/v1/validateFirmware"
)

func TestValidateFirmware(t *testing.T) {
	t.Parallel()
	t.Run("bogus request", func(t *testing.T) {
		t.Parallel()
		//repo, fleetdb, stream, server, err := setupTestServer(t)
		_, _, _, server, err := setupTestServer(t)
		require.NoError(t, err, "prerequisite setup")
		payload := []byte(`{"msg": "this is not a valid firmware validation request"}`)

		req, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, fwValidationUrl, bytes.NewReader(payload))
		require.NoError(t, err, "creating http POST request")

		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		require.Equal(t, http.StatusBadRequest, recorder.Code, "returned payload: %s", string(asBytes(t, recorder.Body)))
	})
	t.Run("bad facility lookup", func(t *testing.T) {
		t.Parallel()
		_, fleetdb, _, server, err := setupTestServer(t)
		require.NoError(t, err, "prerequisite setup")

		fvr, err := FirmwareValidationRequest{
			ServerID:      uuid.New(),
			FirmwareSetID: uuid.New(),
		}.AsJSON()
		require.NoError(t, err, "creating validation request")

		fleetdb.On("GetServer", mock.Anything, mock.Anything).Return(nil, errors.New("pound sand"))

		req, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, fwValidationUrl, bytes.NewReader(fvr))
		require.NoError(t, err, "creating http POST request")

		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		require.Equal(t, http.StatusInternalServerError, recorder.Code, "returned payload: %s", string(asBytes(t, recorder.Body)))
	})
	t.Run("create conditions error", func(t *testing.T) {
		t.Parallel()
		repo, fleetdb, _, server, err := setupTestServer(t)
		require.NoError(t, err, "prerequisite setup")

		srv := &model.Server{
			ID:           uuid.New(),
			FacilityCode: "fc13",
		}

		fvr, err := FirmwareValidationRequest{
			ServerID:      srv.ID,
			FirmwareSetID: uuid.New(),
		}.AsJSON()
		require.NoError(t, err, "creating validation request")

		fleetdb.On("GetServer", mock.Anything, mock.Anything).Return(srv, nil).Twice()
		repo.On("Create", mock.Anything, mock.Anything, "fc13", mock.Anything, mock.Anything, mock.Anything).
			Return(store.ErrActiveCondition).Once()
		repo.On("Create", mock.Anything, mock.Anything, "fc13", mock.Anything, mock.Anything, mock.Anything).
			Return(errors.New("pound sand")).Once()

		req, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, fwValidationUrl, bytes.NewReader(fvr))
		require.NoError(t, err, "creating first http POST request")

		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		require.Equal(t, http.StatusConflict, recorder.Code, "returned payload: %s", string(asBytes(t, recorder.Body)))

		req, err = http.NewRequestWithContext(context.TODO(), http.MethodPost, fwValidationUrl, bytes.NewReader(fvr))
		require.NoError(t, err, "creating second http POST request")

		recorder = httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		require.Equal(t, http.StatusInternalServerError, recorder.Code, "returned payload: %s", string(asBytes(t, recorder.Body)))
	})
	t.Run("publish conditions error", func(t *testing.T) {
		t.Parallel()
		repo, fleetdb, stream, server, err := setupTestServer(t)
		require.NoError(t, err, "prerequisite setup")

		srv := &model.Server{
			ID:           uuid.New(),
			FacilityCode: "fc13",
		}

		fvr, err := FirmwareValidationRequest{
			ServerID:      srv.ID,
			FirmwareSetID: uuid.New(),
		}.AsJSON()
		require.NoError(t, err, "creating validation request")

		fleetdb.On("GetServer", mock.Anything, mock.Anything).Return(srv, nil).Once()

		repo.On("Create", mock.Anything, mock.Anything, "fc13", mock.Anything, mock.Anything, mock.Anything).
			Return(nil).Once()
		repo.On("Update", mock.Anything, srv.ID, mock.Anything).Return(nil).Once()

		stream.On("Publish", mock.Anything, "fc13.servers.firmwareInstall", mock.Anything).
			Return(errors.New("no can do")).Once()

		req, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, fwValidationUrl, bytes.NewReader(fvr))
		require.NoError(t, err, "creating http POST request")

		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		require.Equal(t, http.StatusInternalServerError, recorder.Code, "returned payload: %s", string(asBytes(t, recorder.Body)))
	})
	t.Run("happy path", func(t *testing.T) {
		t.Parallel()
		repo, fleetdb, stream, server, err := setupTestServer(t)
		require.NoError(t, err, "prerequisite setup")

		srv := &model.Server{
			ID:           uuid.New(),
			FacilityCode: "fc13",
		}

		fvr, err := FirmwareValidationRequest{
			ServerID:      srv.ID,
			FirmwareSetID: uuid.New(),
		}.AsJSON()
		require.NoError(t, err, "creating validation request")

		fleetdb.On("GetServer", mock.Anything, mock.Anything).Return(srv, nil).Once()

		repo.On("Create", mock.Anything, mock.Anything, "fc13", mock.Anything, mock.Anything, mock.Anything).
			Return(nil).Once()

		stream.On("Publish", mock.Anything, "fc13.servers.firmwareInstall", mock.Anything).Return(nil).Once()

		req, err := http.NewRequestWithContext(context.TODO(), http.MethodPost, fwValidationUrl, bytes.NewReader(fvr))
		require.NoError(t, err, "creating http POST request")

		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		require.Equal(t, http.StatusOK, recorder.Code, "returned payload: %s", string(asBytes(t, recorder.Body)))
	})
}
