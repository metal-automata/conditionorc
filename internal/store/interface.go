package store

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/app"
	"github.com/metal-automata/conditionorc/internal/model"
	rctypes "github.com/metal-automata/rivets/condition"
	"github.com/metal-automata/rivets/events"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ConditionRecord is a container of multiple conditions all handled as a single
// unit-of-work.
type ConditionRecord struct {
	ID         uuid.UUID            `json:"id"`
	State      rctypes.State        `json:"state"`
	Facility   string               `json:"facility"`
	Conditions []*rctypes.Condition `json:"conditions"`
}

func (c ConditionRecord) MustJSON() json.RawMessage {
	byt, err := json.Marshal(&c)
	if err != nil {
		panic("bad condition record serialize")
	}
	return byt
}

func (c *ConditionRecord) FromJSON(rm json.RawMessage) error {
	err := json.Unmarshal(rm, c)
	if err != nil {
		err = errors.Wrap(errBadData, err.Error())
	}
	return err
}

// NOTE: when updating this interface, run make gen-store-mock to make sure the mocks are updated.
type Repository interface {
	// Get the last condition set on a server in any state, including finished ones.
	// @serverID: required
	Get(ctx context.Context, serverID uuid.UUID) (*ConditionRecord, error)

	// Get the currently active condition. If there is nothing active, return ErrNoConditionFound.
	// @serverID: required
	GetActiveCondition(ctx context.Context, serverID uuid.UUID) (*rctypes.Condition, error)

	// Create a condition record that encapsulates a unit of work, which can encompass one or many conditions.
	// If you create a condition record with 0 conditions, you don't actually create anything, but
	// no error is returned.
	Create(ctx context.Context, serverID uuid.UUID, facilityCode string, conditions ...*rctypes.Condition) error

	// Update a condition on a server
	// @serverID: required
	// @condition: required
	Update(ctx context.Context, serverID uuid.UUID, condition *rctypes.Condition) error

	// List all condition records
	List(ctx context.Context) ([]*ConditionRecord, error)
}

var (
	pkgName            = "internal/store"
	ErrRepository      = errors.New("storage repository error")
	ErrActiveCondition = errors.New("server has an active condition")
)

func NewStore(config *app.Configuration, logger *logrus.Logger, stream events.Stream) (Repository, error) {
	storeKind := strings.ToLower(string(config.StoreKind))

	switch model.StoreKind(storeKind) {
	case model.NATS:
		return newNatsRepository(logger, stream, config.NatsOptions.KVReplicationFactor)
	default:
		return nil, errors.Wrap(ErrRepository, "storage kind not implemented: "+string(config.StoreKind))
	}
}
