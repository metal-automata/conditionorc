package fleetdb

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/app"
	"github.com/metal-automata/conditionorc/internal/metrics"
	"github.com/metal-automata/conditionorc/internal/model"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"

	fleetdbapi "github.com/metal-automata/fleetdb/pkg/api/v1"
	rctypes "github.com/metal-automata/rivets/condition"
)

type fleetDBImpl struct {
	config *app.FleetDBAPIOptions
	client *fleetdbapi.Client
	logger *logrus.Logger
}

var (
	// connectionTimeout is the maximum amount of time spent on each http connection to FleetDBClient.
	connectionTimeout      = 30 * time.Second
	pkgName                = "internal/fleetdb"
	errServerLookup        = errors.New("unable to retrieve server")
	ErrServerNotFound      = errors.New("server not found")
	ErrFirmwareSetNotFound = errors.New("firmware set not found")
	errIncomplete          = errors.New("condition is still active")
)

func fleetdbAPIError(operation string) {
	metrics.DependencyError("fleetdbapi", operation)
}

// AddServer creates a server record in FleetDB
func (s *fleetDBImpl) AddServer(ctx context.Context, serverID uuid.UUID, facilityCode, bmcAddr, bmcUser, bmcPass string) error {
	otelCtx, span := otel.Tracer(pkgName).Start(ctx, "FleetDB.AddServer")
	defer span.End()

	if bmcUser == "" || bmcPass == "" {
		return ErrBMCCredentials
	}

	_, err := netip.ParseAddr(bmcAddr)
	if err != nil {
		return err
	}

	// Add server
	server := fleetdbapi.Server{
		UUID:         serverID,
		Name:         serverID.String(),
		FacilityCode: facilityCode,
		BMC: &fleetdbapi.ServerBMC{
			Username:  bmcUser,
			Password:  bmcPass,
			IPAddress: bmcAddr,
		},
	}

	_, _, err = s.client.Create(otelCtx, server)
	return err
}

// GetServer returns the facility for the requested server id.
func (s *fleetDBImpl) GetServer(ctx context.Context, serverID uuid.UUID) (*model.Server, error) {
	otelCtx, span := otel.Tracer(pkgName).Start(ctx, "FleetDB.GetServer")
	defer span.End()

	// list attributes on a server
	obj, _, err := s.client.GetServer(otelCtx, serverID, &fleetdbapi.ServerGetParams{IncludeBMC: true})
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil, ErrServerNotFound
		}

		s.logger.WithFields(logrus.Fields{
			"serverID": serverID.String(),
			"error":    err,
			"method":   "GetServer",
		}).Warn("error reaching fleetDB")

		fleetdbAPIError("get-server")

		return nil, errors.Wrap(errServerLookup, err.Error())
	}

	return &model.Server{ID: obj.UUID, FacilityCode: obj.FacilityCode}, nil
}

// DeleteServer creates a server record in FleetDB
func (s *fleetDBImpl) DeleteServer(ctx context.Context, serverID uuid.UUID) error {
	otelCtx, span := otel.Tracer(pkgName).Start(ctx, "FleetDB.GetServer")
	defer span.End()

	_, err := s.client.Delete(otelCtx, fleetdbapi.Server{UUID: serverID})
	return err
}

// WriteEventHistory commits a condition in a final state to FleetDB
//
//nolint:revive // calling `fleetDBImpl` s is almost as stupid as changing a bunch of accepted and tested code
func (i *fleetDBImpl) WriteEventHistory(ctx context.Context, cond *rctypes.Condition) error {
	otelCtx, span := otel.Tracer(pkgName).Start(ctx, "FleetDB.WriteEventHistory")
	defer span.End()

	le := i.logger.WithFields(logrus.Fields{
		"condition.id":    cond.ID.String(),
		"server.id":       cond.Target.String(),
		"condition.state": string(cond.State),
		"condition.kind":  string(cond.Kind),
	})

	// only final conditions can be written to history
	if !cond.IsComplete() {
		le.Error("incomplete condition attempted for history")
		return errIncomplete
	}

	lastUpdate := cond.UpdatedAt
	if lastUpdate.IsZero() {
		le.Error("last updated time is zero")
		lastUpdate = time.Now()
	}

	createdTS := cond.CreatedAt

	if createdTS.IsZero() {
		errCreatedAt := errors.New("condition createdAt value is zero")
		le.Error(errCreatedAt)
		return errCreatedAt
	}

	evt := &fleetdbapi.Event{
		EventID:     cond.ID,
		Type:        string(cond.Kind),
		Start:       cond.CreatedAt,
		End:         lastUpdate,
		Target:      cond.Target,
		Parameters:  cond.Parameters,
		FinalState:  string(cond.State),
		FinalStatus: cond.Status,
	}

	_, err := i.client.UpdateEvent(otelCtx, evt)
	if err != nil {
		se := &fleetdbapi.ServerError{}
		if errors.As(err, se) {
			le.WithField("status.code", se.StatusCode)
		}
		le.WithError(err).Warn("updating event history")
	}
	return err
}

// Retrieve a firmware set by its identifier
func (s *fleetDBImpl) FirmwareSetByID(ctx context.Context, id uuid.UUID) (*fleetdbapi.ComponentFirmwareSet, error) {
	otelCtx, span := otel.Tracer(pkgName).Start(ctx, "FleetDB.FirmwareSetByID")
	defer span.End()

	errFirmwareSetIDLookup := errors.New("error in firmware set ID lookup")
	obj, _, err := s.client.GetServerComponentFirmwareSet(otelCtx, id)
	if err != nil {
		se := &fleetdbapi.ServerError{}
		if errors.As(err, se) && se.StatusCode == 404 {
			return nil, ErrFirmwareSetNotFound
		}

		s.logger.WithFields(logrus.Fields{
			"setID":  id.String(),
			"error":  err,
			"method": "FirmwareSetByID",
		}).Warn("FleetDB API query error")

		fleetdbAPIError("get-firmware-set-id")

		return nil, errors.Wrap(errFirmwareSetIDLookup, err.Error())
	}

	return obj, nil
}
