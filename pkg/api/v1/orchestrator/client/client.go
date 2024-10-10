package client

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/orchestrator/types"
	rctypes "github.com/metal-automata/rivets/condition"
)

// Doer performs HTTP requests.
//
// The standard http.Client implements this interface.
type HTTPRequestDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client can perform queries against the Conditions Orchestrator API service.
type Client struct {
	// The Orchestrator API server address with the schema - https://foo.com
	serverAddress string

	// Authentication token
	authToken string

	// Doer for performing requests, typically a *http.Client with any
	// customized settings, such as certificate chains.
	client HTTPRequestDoer
}

// Option allows setting custom parameters during construction
type Option func(*Client) error

// Creates a new Client, with reasonable defaults
func NewClient(serverAddress string, opts ...Option) (Queryor, error) {
	// create a client with sane default values
	client := Client{
		serverAddress: serverAddress,
	}
	// mutate client and add all optional params
	for _, o := range opts {
		if err := o(&client); err != nil {
			return nil, err
		}
	}

	// create httpClient, if not already present
	if client.client == nil {
		client.client = &http.Client{}
	}

	return &client, nil
}

// WithHTTPClient allows overriding the default Doer, which is
// automatically created using http.Client. This is useful for tests.
func WithHTTPClient(doer HTTPRequestDoer) Option {
	return func(c *Client) error {
		c.client = doer
		return nil
	}
}

// WithAuthToken sets the client auth token.
func WithAuthToken(authToken string) Option {
	return func(c *Client) error {
		c.authToken = authToken
		return nil
	}
}

// The Queryor interface enables
type Queryor interface {
	// Retrieve the current in-complete condition for the serverID
	// The returned condition is in either the Active or Pending states.
	ConditionQuery(ctx context.Context, serverID uuid.UUID) (*v1types.ServerResponse, error)
	// Update a Condition status
	ConditionStatusUpdate(ctx context.Context, conditionKind rctypes.Kind, serverID, conditionID uuid.UUID, statusValue *rctypes.StatusValue, onlyUpdateTimestamp bool) (*v1types.ServerResponse, error)
	// Publish Task
	ConditionTaskPublish(ctx context.Context, conditionKind rctypes.Kind, serverID, conditionID uuid.UUID, task *rctypes.Task[any, any], onlyUpdateTimestamp bool) (*v1types.ServerResponse, error)
	// Query task
	ConditionTaskQuery(ctx context.Context, conditionKind rctypes.Kind, serverID uuid.UUID) (*v1types.ServerResponse, error)
}

func (c *Client) ConditionQuery(ctx context.Context, serverID uuid.UUID) (*v1types.ServerResponse, error) {
	path := fmt.Sprintf("servers/%s/condition", serverID.String())

	return c.get(ctx, path)
}

func (c *Client) ConditionStatusUpdate(ctx context.Context, conditionKind rctypes.Kind, serverID, conditionID uuid.UUID, statusValue *rctypes.StatusValue, onlyUpdateTimestamp bool) (*v1types.ServerResponse, error) {
	path := fmt.Sprintf(
		"servers/%s/condition-status/%s/%s",
		serverID.String(),
		conditionKind,
		conditionID.String(),
	)

	if onlyUpdateTimestamp {
		path += "?ts_update=true"
	} else if statusValue == nil {
		statusValue = &rctypes.StatusValue{}
	}

	return c.put(ctx, path, statusValue)
}

func (c *Client) ConditionTaskPublish(ctx context.Context, conditionKind rctypes.Kind, serverID, conditionID uuid.UUID, task *rctypes.Task[any, any], onlyUpdateTimestamp bool) (*v1types.ServerResponse, error) {
	path := fmt.Sprintf("servers/%s/condition-task/%s/%s", serverID.String(), conditionKind, conditionID.String())

	if onlyUpdateTimestamp {
		path += "?ts_update=true"
	}

	return c.post(ctx, path, task)
}

func (c *Client) ConditionTaskQuery(ctx context.Context, conditionKind rctypes.Kind, serverID uuid.UUID) (*v1types.ServerResponse, error) {
	path := fmt.Sprintf("servers/%s/condition-task/%s", serverID.String(), conditionKind)

	return c.get(ctx, path)
}
