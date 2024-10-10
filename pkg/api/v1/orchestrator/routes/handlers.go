package routes

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/metal-automata/conditionorc/internal/store"
	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/orchestrator/types"
	rctypes "github.com/metal-automata/rivets/condition"
)

var (
	errPublishStatus = errors.New("error in condition status publish")
	errUnmarshalKey  = errors.New("error unmarshal key, value for update")
)

func (r *Routes) conditionKindValid(kind rctypes.Kind) bool {
	found := r.conditionDefinitions.FindByKind(kind)
	return found != nil
}

// @Summary ConditionStatusUpdate
// @Tag Conditions
// @Description Publishes an update to the Condition StatusValue KV
// @ID conditionStatusUpdate
// @Param uuid path string true "Server ID"
// @Param conditionKind path string true "Condition Kind"
// @Param conditionID path string true "Condition ID"
// @Param ts_update query string false "Only update timestamp in the StatusValue entry"
// @Accept json
// @Produce json
// @Success 200 {object} v1types.ServerResponse
// Failure 400 {object} v1types.ServerResponse
// Failure 404 {object} v1types.ServerResponse
// Failure 500 {object} v1types.ServerResponse
// @Router /servers/{uuid}/condition-status/{conditionKind}/{conditionID} [put]
func (r *Routes) conditionStatusUpdate(c *gin.Context) (int, *v1types.ServerResponse) {
	ctx, span := otel.Tracer(pkgName).Start(c.Request.Context(), "Routes.conditionStatusUpdate")
	span.SetAttributes(
		attribute.KeyValue{Key: "serverId", Value: attribute.StringValue(c.Param("uuid"))},
		attribute.KeyValue{Key: "conditionKind", Value: attribute.StringValue(c.Param("conditionKind"))},
		attribute.KeyValue{Key: "conditionID", Value: attribute.StringValue(c.Param("conditionID"))},
		attribute.KeyValue{Key: "timestampUpdate", Value: attribute.StringValue(c.Request.URL.Query().Get("ts_update"))},
	)
	defer span.End()

	serverID, err := uuid.Parse(c.Param("uuid"))
	if err != nil {
		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "invalid server id: " + err.Error(),
		}
	}

	var statusValue rctypes.StatusValue
	var onlyTimestampUpdate bool
	if c.Request.URL.Query().Get("ts_update") == "true" {
		onlyTimestampUpdate = true
	} else {
		if errBind := c.ShouldBindJSON(&statusValue); errBind != nil {
			r.logger.WithError(err).Warn("unmarshal StatusValue payload")

			return http.StatusBadRequest, &v1types.ServerResponse{
				Message: "invalid StatusValue payload: " + errBind.Error(),
			}
		}
	}

	conditionKind := rctypes.Kind(c.Param("conditionKind"))
	if !r.conditionKindValid(conditionKind) {
		r.logger.WithFields(logrus.Fields{
			"kind": conditionKind,
		}).Info("unsupported condition kind")

		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "unsupported condition kind: " + string(conditionKind),
		}
	}

	conditionID, err := uuid.Parse(c.Param("conditionID"))
	if err != nil {
		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "invalid conditionID: " + err.Error(),
		}
	}

	// expect condition to be in an incomplete state to accept this update.
	cond, err := r.repository.GetActiveCondition(ctx, serverID)
	if err != nil {
		if errors.Is(err, store.ErrConditionNotFound) {
			return http.StatusNotFound, &v1types.ServerResponse{
				Message: err.Error(),
			}
		}

		return http.StatusInternalServerError, &v1types.ServerResponse{
			Message: "condition lookup: " + err.Error(),
		}
	}

	if cond.ID != conditionID {
		err := errors.New("update denied, condition ID does not match")
		r.logger.WithError(err).WithFields(
			logrus.Fields{"current conditionID": cond.ID, "request conditionID": conditionID},
		).Warn("incorrect condition update request")

		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: err.Error(),
		}
	}

	if rctypes.StateIsComplete(cond.State) {
		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "update denied, condition in final state: " + string(cond.State),
		}
	}

	if cond.Kind != conditionKind {
		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "no matching condition found in record: " + string(conditionKind),
		}
	}

	// publish status value update
	if err := r.statusValueKV.publish(
		r.facilityCode,
		conditionID,
		conditionKind,
		&statusValue,
		onlyTimestampUpdate,
	); err != nil {
		return http.StatusInternalServerError, &v1types.ServerResponse{
			Message: "status publish error: " + err.Error(),
		}
	}

	return http.StatusOK, &v1types.ServerResponse{
		Message: "condition status update published",
	}
}

// @Summary taskQuery returns the active/pending condition for a serverID
// @Tag Conditions
// @Description Queries a *rivets.Task object from KV for a condition
// @Description Controllers will not have always know the taskID and so this enables querying
// @Description the active Task object using the serverID, conditionKind parameters.
// @ID taskQuery
// @Param uuid path string true "Server ID"
// @Param conditionKind path string true "Condition Kind"
// @Accept json
// @Produce json
// @Success 200 {object} v1types.ServerResponse
// Failure 400 {object} v1types.ServerResponse
// Failure 404 {object} v1types.ServerResponse
// Failure 422 {object} v1types.ServerResponse
// Failure 500 {object} v1types.ServerResponse
// @Router /servers/{uuid}/condition-task/{conditionKind} [get]
func (r *Routes) taskQuery(c *gin.Context) (int, *v1types.ServerResponse) {
	ctx, span := otel.Tracer(pkgName).Start(c.Request.Context(), "Routes.taskQuery")
	span.SetAttributes(
		attribute.KeyValue{Key: "serverId", Value: attribute.StringValue(c.Param("uuid"))},
		attribute.KeyValue{Key: "conditionKind", Value: attribute.StringValue(c.Param("conditionKind"))},
	)
	defer span.End()

	serverID, err := uuid.Parse(c.Param("uuid"))
	if err != nil {
		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "invalid server id: " + err.Error(),
		}
	}

	conditionKind := rctypes.Kind(c.Param("conditionKind"))
	if !r.conditionKindValid(conditionKind) {
		r.logger.WithFields(logrus.Fields{
			"kind": conditionKind,
		}).Info("unsupported condition kind")

		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "unsupported condition kind: " + string(conditionKind),
		}
	}

	found, err := r.repository.GetActiveCondition(ctx, serverID)
	if err != nil {
		if errors.Is(err, store.ErrConditionNotFound) {
			return http.StatusNotFound, &v1types.ServerResponse{
				Message: "no pending/active condition not found for server",
			}
		}

		r.logger.WithError(err).Info("condition record query error")

		return http.StatusInternalServerError, &v1types.ServerResponse{
			Message: "condition lookup: " + err.Error(),
		}
	}

	task, err := r.taskKV.get(c.Request.Context(), conditionKind, found.ID, serverID)
	if err != nil {
		if errors.Is(err, errStaleTask) {
			// 422 indicates a stale task for this server in the KV and unless that is purged, we cannot proceed,
			// an operator is required to clean up the stale task.
			return http.StatusUnprocessableEntity, &v1types.ServerResponse{
				Message: err.Error(),
			}
		}

		r.logger.WithField("conditionID", found.ID).WithError(err).Info("task KV query error")
		return http.StatusInternalServerError, &v1types.ServerResponse{
			Message: err.Error(),
		}
	}

	return http.StatusOK, &v1types.ServerResponse{
		Message: "Task identified",
		Task:    task,
	}
}

// @Summary taskPublish
// @Tag Conditions
// @Description Publishes a *rivets.Task object to the KV for a condition
// @ID taskPublish
// @Param uuid path string true "Server ID"
// @Param conditionKind path string true "Condition Kind"
// @Param conditionID path string true "Condition ID"
// @Accept json
// @Produce json
// @Success 200 {object} v1types.ServerResponse
// Failure 400 {object} v1types.ServerResponse
// Failure 404 {object} v1types.ServerResponse
// Failure 503 {object} v1types.ServerResponse
// @Router /servers/{uuid}/condition-task/{conditionKind}/{conditionID} [post]
func (r *Routes) taskPublish(c *gin.Context) (int, *v1types.ServerResponse) {
	ctx, span := otel.Tracer(pkgName).Start(c.Request.Context(), "Routes.taskPublish")
	span.SetAttributes(
		attribute.KeyValue{Key: "serverId", Value: attribute.StringValue(c.Param("uuid"))},
		attribute.KeyValue{Key: "conditionKind", Value: attribute.StringValue(c.Param("conditionKind"))},
		attribute.KeyValue{Key: "conditionID", Value: attribute.StringValue(c.Param("conditionID"))},
		attribute.KeyValue{Key: "timestampUpdate", Value: attribute.StringValue(c.Request.URL.Query().Get("ts_update"))},
	)
	defer span.End()

	var task rctypes.Task[any, any]
	var onlyTimestampUpdate bool

	if c.Request.URL.Query().Get("ts_update") == "true" {
		onlyTimestampUpdate = true
	} else {
		if err := c.ShouldBindJSON(&task); err != nil {
			r.logger.WithError(err).Warn("unmarshal Task payload")

			return http.StatusBadRequest, &v1types.ServerResponse{
				Message: "invalid Task payload: " + err.Error(),
			}
		}
	}

	serverID, err := uuid.Parse(c.Param("uuid"))
	if err != nil {
		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "invalid server id: " + err.Error(),
		}
	}

	conditionKind := rctypes.Kind(c.Param("conditionKind"))
	if !r.conditionKindValid(conditionKind) {
		r.logger.WithFields(logrus.Fields{
			"kind": conditionKind,
		}).Info("unsupported condition kind")

		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "unsupported condition kind: " + string(conditionKind),
		}
	}

	conditionID, err := uuid.Parse(c.Param("conditionID"))
	if err != nil {
		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "invalid conditionID: " + err.Error(),
		}
	}

	// the controller retrieved the condition from the queue which created the Task entry
	// we expect an active/pending condition to allow this publish
	activeCond, err := r.repository.GetActiveCondition(ctx, serverID)
	if err != nil {
		if errors.Is(err, store.ErrConditionNotFound) {
			return http.StatusNotFound, &v1types.ServerResponse{
				Message: err.Error(),
			}
		}

		return http.StatusInternalServerError, &v1types.ServerResponse{
			Message: "condition lookup: " + err.Error(),
		}
	}

	if !onlyTimestampUpdate && activeCond.ID != task.ID {
		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: fmt.Sprintf("TaskID: %s does not match active ConditionID: %s", task.ID, activeCond.ID),
		}
	}

	// publish Task
	if err := r.taskKV.publish(
		ctx,
		serverID.String(),
		conditionID.String(),
		conditionKind,
		&task,
		onlyTimestampUpdate,
	); err != nil {
		return http.StatusInternalServerError, &v1types.ServerResponse{
			Message: "Task publish error: " + err.Error(),
		}
	}

	return http.StatusOK, &v1types.ServerResponse{
		Message: "condition Task published",
	}
}

// @Summary ConditionGet
// @Tag Conditions
// @Description Returns the active/pending Condition for the serverID.
// @ID conditionGet
// @Param uuid path string true "Server ID"
// @Param conditionKind path string true "Condition Kind"
// @Accept json
// @Produce json
// @Success 200 {object} v1types.ServerResponse
// Failure 400 {object} v1types.ServerResponse
// Failure 404 {object} v1types.ServerResponse
// Failure 500 {object} v1types.ServerResponse
// @Router /servers/{uuid}/condition [get]
func (r *Routes) conditionGet(c *gin.Context) (int, *v1types.ServerResponse) {
	ctx, span := otel.Tracer(pkgName).Start(c.Request.Context(), "Routes.conditionGet")
	span.SetAttributes(
		attribute.KeyValue{Key: "serverId", Value: attribute.StringValue(c.Param("uuid"))},
	)
	defer span.End()

	serverID, err := uuid.Parse(c.Param("uuid"))
	if err != nil {
		return http.StatusBadRequest, &v1types.ServerResponse{
			Message: "invalid server id: " + err.Error(),
		}
	}

	found, err := r.repository.GetActiveCondition(ctx, serverID)
	if err != nil {
		if errors.Is(err, store.ErrConditionNotFound) {
			return http.StatusNotFound, &v1types.ServerResponse{
				Message: "no pending/active condition not found for server",
			}
		}

		r.logger.WithError(err).Info("condition record query error")

		return http.StatusInternalServerError, &v1types.ServerResponse{
			Message: "condition lookup: " + err.Error(),
		}
	}

	return http.StatusOK, &v1types.ServerResponse{
		Condition: found,
		Message:   "found condition in state: " + string(found.State),
	}
}
