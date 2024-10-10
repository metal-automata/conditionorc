package routes

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	rctypes "github.com/metal-automata/rivets/condition"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/metal-automata/conditionorc/internal/metrics"
	"github.com/metal-automata/conditionorc/internal/store"
	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/conditions/types"
)

type FirmwareValidationRequest struct {
	ServerID      uuid.UUID `json:"server_id" binding:"required,uuid4_rfc4122"`
	FirmwareSetID uuid.UUID `json:"firmware_set_id" binding:"required,uuid4_rfc4122"`
}

func (fvr FirmwareValidationRequest) AsJSON() (json.RawMessage, error) {
	return json.Marshal(fvr)
}

// this is where we compose all conditions to be executed during the firmware validation task
func firmwareValidationConditions(fvr FirmwareValidationRequest) *rctypes.ServerConditions {
	createTime := time.Now()

	fwParams := &rctypes.FirmwareInstallTaskParameters{
		FirmwareSetID:         fvr.FirmwareSetID,
		AssetID:               fvr.ServerID,
		ResetBMCBeforeInstall: true,
	}

	validationParams := &rctypes.ServerControlTaskParameters{
		AssetID:                 fvr.ServerID,
		Action:                  rctypes.ValidateFirmware,
		ValidateFirmwareTimeout: 30 * time.Minute,
		ValidateFirmwareID:      fvr.FirmwareSetID,
	}

	return &rctypes.ServerConditions{
		ServerID: fvr.ServerID,
		Conditions: []*rctypes.Condition{
			{
				Kind:       rctypes.FirmwareInstall,
				Version:    rctypes.ConditionStructVersion,
				Parameters: fwParams.MustJSON(),
				State:      rctypes.Pending,
				CreatedAt:  createTime,
			},
			{
				Kind:       rctypes.Inventory,
				Version:    rctypes.ConditionStructVersion,
				Parameters: rctypes.MustDefaultInventoryJSON(fvr.ServerID),
				State:      rctypes.Pending,
				CreatedAt:  createTime,
			},
			{
				Kind:       rctypes.ServerControl,
				Version:    rctypes.ConditionStructVersion,
				Parameters: validationParams.MustJSON(),
				State:      rctypes.Pending,
				CreatedAt:  createTime,
			},
		},
	}
}

// @Summary Validate Firmware
// @Tag Conditions
// @Description Initiates a firmware install, an inventory, and a firmware validation in a single workflow.
// @Param data body FirmwareValidationRequest true "firmware validation options: server id and firmware set id"
// @Accept json
// @Produce json
// @Success 200 {object} v1types.ServerResponse
// Failure 400 {object} v1types.ServerResponse
// Failure 403 {object} v1types.ServerResponse
// Failure 409 {object} v1types.ServerResponse
// Failure 503 {object} v1types.ServerResponse
// @Router /validateFirmware [post]
func (r *Routes) validateFirmware(c *gin.Context) (int, *v1types.ServerResponse) {
	otelCtx, span := otel.Tracer(pkgName).Start(c.Request.Context(), "Routes.validateFirmware")
	defer span.End()

	var fvr FirmwareValidationRequest
	if err := c.ShouldBindJSON(&fvr); err != nil {
		r.logger.WithError(err).Warn("unmarshal firmware validation payload")

		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "invalid firmware validation payload: " + err.Error(),
		}
	}

	facilityCode, err := r.serverFacilityCode(otelCtx, fvr.ServerID)
	if err != nil {
		return http.StatusInternalServerError, &v1types.ServerResponse{
			Message: "server facility: " + err.Error(),
		}
	}

	span.SetAttributes(
		attribute.KeyValue{Key: "serverId", Value: attribute.StringValue(fvr.ServerID.String())},
		attribute.KeyValue{Key: "firmwareSet", Value: attribute.StringValue(fvr.ServerID.String())},
		attribute.KeyValue{Key: "facility", Value: attribute.StringValue(facilityCode)},
	)

	conds := firmwareValidationConditions(fvr)
	// XXX: This is lifted from the firmwareInstall api code. Consider abstracting a utility method for
	// Create and publishCondition.
	if err = r.repository.Create(otelCtx, fvr.ServerID, facilityCode, conds.Conditions...); err != nil {
		if errors.Is(err, store.ErrActiveCondition) {
			return http.StatusConflict, &v1types.ServerResponse{
				Message: err.Error(),
			}
		}

		return http.StatusInternalServerError, &v1types.ServerResponse{
			Message: "scheduling condition: " + err.Error(),
		}
	}

	if err = r.publishCondition(otelCtx, fvr.ServerID, facilityCode, conds.Conditions[0]); err != nil {
		r.logger.WithField("kind", conds.Conditions[0].Kind).WithError(err).Warn("error publishing condition")
		// mark first condition as failed
		conds.Conditions[0].State = rctypes.Failed
		conds.Conditions[0].Status = failedPublishStatus

		if markErr := r.repository.Update(otelCtx, fvr.ServerID, conds.Conditions[0]); markErr != nil {
			// an operator is going to have to sort this out
			r.logger.WithError(err).Warn("marking unpublished condition failed")
		}
		return http.StatusInternalServerError, &v1types.ServerResponse{
			Message: "publishing condition" + err.Error(),
		}
	}

	metrics.ConditionQueued.With(
		// XXX: define a Kind for firmwareValidation
		prometheus.Labels{"conditionKind": string(rctypes.ValidateFirmware)},
	).Inc()

	return http.StatusOK, &v1types.ServerResponse{
		Message: "firmware validation scheduled",
		Records: &v1types.ConditionsResponse{
			ServerID:   fvr.ServerID,
			State:      rctypes.Pending,
			Conditions: conds.Conditions,
		},
	}
}
