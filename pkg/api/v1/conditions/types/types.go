package types

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	rctypes "github.com/metal-automata/rivets/condition"
	"github.com/pkg/errors"

	"github.com/metal-automata/rivets/events/registry"
)

var (
	errConditionMerge         error = errors.New("condition merge error")
	errInvalidStateTransition error = errors.New("invalid state transition")

	ErrUpdatePayload error = errors.New("invalid payload for update")
)

type ServerResponse struct {
	StatusCode int                 `json:"statusCode,omitempty"`
	Message    string              `json:"message,omitempty"`
	Records    *ConditionsResponse `json:"records,omitempty"`
}

// ConditionsResponse is the response returned for listing multiple conditions on a server.
type ConditionsResponse struct {
	ServerID   uuid.UUID            `json:"serverID,omitempty"`
	State      rctypes.State        `json:"state,omitempty"`
	Conditions []*rctypes.Condition `json:"conditions,omitempty"`
}

// ConditionCreate is the request payload to create a condition with its parameters on server.
type ConditionCreate struct {
	Parameters json.RawMessage `json:"parameters"`
	Fault      *rctypes.Fault  `json:"fault,omitempty"`
}

// AddServerParams is the request payload to add a server to fleetdb.
type AddServerParams struct {
	Facility       string `json:"facility"`
	IP             string `json:"bmc-ip"`
	Username       string `json:"bmc-user"`
	Password       string `json:"bmc-pwd"`
	HardwareVendor string `json:"hardware-vendor"`
	HardwareModel  string `json:"hardware-model"`
}

func (asp *AddServerParams) MustJSON() []byte {
	byt, err := json.Marshal(asp)
	if err != nil {
		panic(err)
	}
	return byt
}

// NewCondition returns a new Condition type.
func (c *ConditionCreate) NewCondition(kind rctypes.Kind, target uuid.UUID) *rctypes.Condition {
	return &rctypes.Condition{
		ID:         uuid.New(),
		Target:     target,
		Version:    rctypes.ConditionStructVersion,
		Kind:       kind,
		State:      rctypes.Pending,
		Parameters: c.Parameters,
		Fault:      c.Fault,
		CreatedAt:  time.Now(),
	}
}

// ConditionUpdate is the request payload to update an existing rctypes.
type ConditionUpdate struct {
	ConditionID uuid.UUID       `json:"conditionID"`
	ServerID    uuid.UUID       `json:"serverID"`
	State       rctypes.State   `json:"state,omitempty"`
	Status      json.RawMessage `json:"status,omitempty"`
	UpdatedAt   time.Time       `json:"updatedAt,omitempty"`
	CreatedAt   time.Time       `json:"createdAt,omitempty"`
}

func (c *ConditionUpdate) Validate() error {
	if c.ConditionID == uuid.Nil {
		return errors.Wrap(ErrUpdatePayload, "ConditionID not set")
	}

	if c.ServerID == uuid.Nil {
		return errors.Wrap(ErrUpdatePayload, "ServerID not set")
	}

	if c.State == "" || c.Status == nil {
		return errors.Wrap(ErrUpdatePayload, "state and status attributes are expected")
	}

	return nil
}

// ConditionUpdateEvent is the payload received for a condition update over the event stream.
type ConditionUpdateEvent struct {
	ConditionUpdate
	Kind                  rctypes.Kind `json:"kind"`
	registry.ControllerID `json:"controllerID"`
}

// Validate checks for required attributes.
//
// Note:
// The ResourceVersion attribute is not validated for updates through events,
// this is because the controllers do not perform requests for the existing condition
// since implementing a Request-Reply pattern on the NATS Jetstream is tideous and not recommended.
//
// The NATS Jetstream guarantees ordered delivery, as long as there is a single consumer of the event,
// for now we're deploying a single orchestrator instance in each facility and so this check is not required.
// In the case that we require multiple orchestrators in a facility the stream would need to be partitioned
// to ensure ordered delivery.
//
// ref:
// https://github.com/nats-io/nats.py/discussions/221
// https://github.com/nats-io/nats.go/discussions/970#discussioncomment-2690789
//
// TODO: move this note into the messaging architecture doc.
func (c *ConditionUpdateEvent) Validate() error {
	if c.Kind == "" {
		return errors.Wrap(ErrUpdatePayload, "Kind attribute expected")
	}

	return c.ConditionUpdate.Validate()
}

// MergeExisting when given an existing condition, validates the update based on existing values
// and returns a condition that can be passed to the repository for update.
//
// The resourceVersion is not updated here and is left for the repository Store to update.
//
// This method makes sure that update does not overwrite existing data inadvertently.
func (c *ConditionUpdateEvent) MergeExisting(existing *rctypes.Condition) (*rctypes.Condition, error) {
	// condition must already exist for update.
	if existing == nil {
		return nil, errors.Wrap(errConditionMerge, "existing condition is nil")
	}

	// condition identifier must match
	if existing.ID != c.ConditionID {
		return nil, errors.Wrap(errConditionMerge, "Condition ID in update does not match existing")
	}

	// kind must match
	if existing.Kind != c.Kind {
		return nil, errors.Wrap(errConditionMerge, "update kind does not match existing")
	}

	// transition is valid
	if !existing.State.TransitionValid(c.State) {
		return nil, errInvalidStateTransition
	}

	// target identifiers must match
	if existing.Target != c.ServerID {
		return nil, errors.Wrap(errConditionMerge, "ServerID in update does not match existing")
	}

	return &rctypes.Condition{
		Version:               existing.Version,
		ID:                    existing.ID,
		Kind:                  existing.Kind,
		Parameters:            existing.Parameters,
		State:                 c.State,
		Status:                c.Status,
		Target:                existing.Target,
		FailOnCheckpointError: existing.FailOnCheckpointError,
		UpdatedAt:             c.UpdatedAt,
		CreatedAt:             existing.CreatedAt,
	}, nil
}

// ServerProvisionRequest is the request payload to provision a server.
// Duplicated from FCP without Spec: https://github.com/equinixmetal/facility-controlplane/blob/main/pkg/api/v1/types.go#L388-L402
type ServerProvisionRequest struct {
	ServerID             uuid.UUID `json:"serverID,omitempty"`
	InstanceID           uuid.UUID `json:"instanceID,omitempty"`
	Hostname             string    `json:"Hostname,omitempty"`
	ProviderType         string    `json:"ProviderType,omitempty"`
	OperatingSystemID    string    `json:"OperatingSystemID,omitempty"`
	UserData             string    `json:"UserData,omitempty"`
	StorageConfiguration string    `json:"StorageConfiguration,omitempty"`
	NetworkConfiguration string    `json:"NetworkConfiguration,omitempty"`
	Tags                 []string  `json:"Tags,omitempty"`
}
