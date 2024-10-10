package routes

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/metal-automata/conditionorc/internal/status"
	"github.com/nats-io/nats.go"
	"github.com/pkg/errors"

	rctypes "github.com/metal-automata/rivets/condition"
)

type statusValueKV interface {
	publish(
		facilityCode string,
		conditionID uuid.UUID,
		conditionKind rctypes.Kind,
		newSV *rctypes.StatusValue,
		onlyTimestamp bool,
	) error
}

// statusValue implements the statusValueKV interface
type statusValue struct{}

func initStatusValueKV() statusValueKV {
	return &statusValue{}
}

func (s *statusValue) publish(
	facilityCode string,
	conditionID uuid.UUID,
	conditionKind rctypes.Kind,
	newSV *rctypes.StatusValue,
	onlyTimestamp bool,
) error {
	statusKV, err := status.GetConditionKV(conditionKind)
	if err != nil {
		return errors.Wrap(errPublishStatus, err.Error())
	}

	key := rctypes.StatusValueKVKey(facilityCode, conditionID.String())
	currEntry, err := statusKV.Get(key)

	create := func() error {
		newSV.CreatedAt = time.Now()
		if _, errCreate := statusKV.Create(key, newSV.MustBytes()); errCreate != nil {
			return errors.Wrap(errPublishStatus, errCreate.Error())
		}

		return nil
	}

	if err != nil {
		if errors.Is(err, nats.ErrKeyNotFound) {
			return create()
		}

		return errors.Wrap(errPublishStatus, err.Error())
	}

	// only timestamp update takes the current value
	var publishSV *rctypes.StatusValue
	if onlyTimestamp {
		curSV := &rctypes.StatusValue{}
		if errJSON := json.Unmarshal(currEntry.Value(), &curSV); errJSON != nil {
			return errors.Wrap(errUnmarshalKey, errJSON.Error())
		}

		publishSV = curSV
	} else {
		if newSV == nil {
			return errors.Wrap(errPublishStatus, "expected a StatusValue param got nil")
		}

		publishSV = newSV
	}

	publishSV.UpdatedAt = time.Now()
	if _, err := statusKV.Update(key, publishSV.MustBytes(), currEntry.Revision()); err != nil {
		return errors.Wrap(errPublishStatus, err.Error())
	}

	return nil
}
