package types

import (
	rctypes "github.com/metal-automata/rivets/condition"
)

type ServerResponse struct {
	StatusCode int                     `json:"statusCode,omitempty"`
	Message    string                  `json:"message,omitempty"`
	Condition  *rctypes.Condition      `json:"condition,omitempty"`
	Task       *rctypes.Task[any, any] `json:"task,omitempty"`
}
