package conditions

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/conditions/types"
	rctypes "github.com/metal-automata/rivets/condition"
)

// Doer performs HTTP requests.
//
// The standard http.Client implements this interface.
type HTTPRequestDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client can perform queries against the Conditions API service.
type Client struct {
	// The Conditions API server address with the schema - https://foo.com
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
func NewClient(serverAddress string, opts ...Option) (*Client, error) {
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

func (c *Client) ServerConditionStatus(ctx context.Context, serverID uuid.UUID) (*v1types.ServerResponse, error) {
	path := fmt.Sprintf("servers/%s/status", serverID.String())

	return c.get(ctx, path)
}

func (c *Client) ServerFirmwareInstall(ctx context.Context,
	params *rctypes.FirmwareInstallTaskParameters) (*v1types.ServerResponse, error) {
	path := fmt.Sprintf("servers/%s/firmwareInstall", params.AssetID.String())

	return c.post(ctx, path, params)
}

func (c *Client) ServerBiosControl(ctx context.Context,
	params *rctypes.BiosControlTaskParameters) (*v1types.ServerResponse, error) {
	path := fmt.Sprintf("servers/%s/biosControl", params.AssetID.String())

	return c.post(ctx, path, params)
}

func (c *Client) ServerConditionCreate(ctx context.Context, serverID uuid.UUID, conditionKind rctypes.Kind, conditionCreate v1types.ConditionCreate) (*v1types.ServerResponse, error) {
	path := fmt.Sprintf("servers/%s/condition/%s", serverID.String(), conditionKind)

	return c.post(ctx, path, conditionCreate)
}

func (c *Client) ServerEnroll(ctx context.Context, serverID string, conditionCreate v1types.ConditionCreate) (*v1types.ServerResponse, error) {
	// Empty server ID is allowed. Conditionorc will create a new server ID if it is empty.
	path := fmt.Sprintf("serverEnroll/%s", serverID)

	return c.post(ctx, path, conditionCreate)
}

func (c *Client) ServerDelete(ctx context.Context, serverID string) (*v1types.ServerResponse, error) {
	path := fmt.Sprintf("servers/%s", serverID)

	return c.delete(ctx, path)
}
