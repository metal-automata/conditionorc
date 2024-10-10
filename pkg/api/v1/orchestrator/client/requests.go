package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/metal-automata/conditionorc/internal/version"
	"github.com/metal-automata/conditionorc/pkg/api/v1/orchestrator/routes"
	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/orchestrator/types"
)

func (c *Client) get(ctx context.Context, path string) (*v1types.ServerResponse, error) {
	requestURL, err := url.Parse(fmt.Sprintf("%s%s/%s", c.serverAddress, routes.PathPrefix, path))
	if err != nil {
		return nil, Error{Cause: err.Error()}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), http.NoBody)
	if err != nil {
		return nil, Error{Cause: "error in GET request" + err.Error()}
	}

	return c.do(req)
}

func (c *Client) put(ctx context.Context, path string, body interface{}) (*v1types.ServerResponse, error) {
	requestURL, err := url.Parse(fmt.Sprintf("%s%s/%s", c.serverAddress, routes.PathPrefix, path))
	if err != nil {
		return nil, Error{Cause: err.Error()}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, Error{Cause: "error in PUT JSON payload: " + err.Error()}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, requestURL.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, Error{Cause: "error in PUT request" + err.Error()}
	}

	return c.do(req)
}

func (c *Client) post(ctx context.Context, path string, body interface{}) (*v1types.ServerResponse, error) {
	requestURL, err := url.Parse(fmt.Sprintf("%s%s/%s", c.serverAddress, routes.PathPrefix, path))
	if err != nil {
		return nil, Error{Cause: err.Error()}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, Error{Cause: "error in POST JSON payload: " + err.Error()}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, Error{Cause: "error in POST request" + err.Error()}
	}

	return c.do(req)
}

func (c *Client) do(req *http.Request) (*v1types.ServerResponse, error) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("condition-orc-client (%s)", version.Current().String()))

	if c.authToken != "" {
		req.Header.Set("Authorization", fmt.Sprintf("bearer %s", c.authToken))
	}

	response, err := c.client.Do(req)
	if err != nil {
		return nil, RequestError{err.Error(), c.statusCode(response)}
	}

	if response == nil {
		return nil, RequestError{"got empty response body", 0}
	}

	defer response.Body.Close()

	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, RequestError{
			"failed to read response body: " + err.Error(),
			c.statusCode(response),
		}
	}

	serverResponse := &v1types.ServerResponse{}

	if err := json.Unmarshal(data, &serverResponse); err != nil {
		return nil, RequestError{
			"failed to unmarshal response from server: " + err.Error(),
			c.statusCode(response),
		}
	}

	serverResponse.StatusCode = response.StatusCode

	return serverResponse, nil
}

func (c *Client) statusCode(response *http.Response) int {
	if response != nil {
		return response.StatusCode
	}

	return 0
}
