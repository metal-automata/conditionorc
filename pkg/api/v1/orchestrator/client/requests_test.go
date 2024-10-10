// nolint
package client

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	v1types "github.com/metal-automata/conditionorc/pkg/api/v1/orchestrator/types"
	"github.com/stretchr/testify/assert"
)

func TestClientDo(t *testing.T) {
	tests := []struct {
		name               string
		setupClient        func() *Client
		setupRequest       func() *http.Request
		mockResponse       *http.Response
		mockError          error
		expectedResponse   *v1types.ServerResponse
		expectedError      error
		expectedStatusCode int
		checkHeaders       func(*testing.T, http.Header)
	}{
		{
			name: "Request with auth token",
			setupClient: func() *Client {
				return &Client{
					serverAddress: "https://foo",
					authToken:     "hunter2",
				}
			},
			setupRequest: func() *http.Request {
				req, _ := http.NewRequest("GET", "https://foo/api", http.NoBody)
				return req
			},
			mockResponse: &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"message": "success"}`)),
			},
			expectedResponse: &v1types.ServerResponse{
				Message:    "success",
				StatusCode: http.StatusOK,
			},
			expectedStatusCode: http.StatusOK,
			checkHeaders: func(t *testing.T, h http.Header) {
				assert.Equal(t, "application/json", h.Get("Content-Type"))
				assert.Equal(t, "bearer hunter2", h.Get("Authorization"))
			},
		},
		{
			name: "Request error",
			setupClient: func() *Client {
				return &Client{
					serverAddress: "https://foo",
				}
			},
			setupRequest: func() *http.Request {
				req, _ := http.NewRequest("GET", "https://foo/api", http.NoBody)
				return req
			},
			mockError:          errors.New("network timeout"),
			expectedError:      RequestError{"network timeout", 0},
			expectedStatusCode: 0,
		},
		{
			name: "Empty response body",
			setupClient: func() *Client {
				return &Client{
					serverAddress: "https://foo",
				}
			},
			setupRequest: func() *http.Request {
				req, _ := http.NewRequest("GET", "https://foo/api", http.NoBody)
				return req
			},
			mockResponse: &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
			},
			expectedError:      RequestError{"failed to unmarshal response from server: unexpected end of JSON input", http.StatusOK},
			expectedStatusCode: http.StatusOK,
		},
		{
			name: "Request without auth token",
			setupClient: func() *Client {
				return &Client{
					serverAddress: "https://foo",
				}
			},
			setupRequest: func() *http.Request {
				req, _ := http.NewRequest("GET", "https://foo/api", http.NoBody)
				return req
			},
			mockResponse: &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"message": "success"}`)),
			},
			expectedResponse: &v1types.ServerResponse{
				Message:    "success",
				StatusCode: http.StatusOK,
			},
			expectedStatusCode: http.StatusOK,
			checkHeaders: func(t *testing.T, h http.Header) {
				assert.Equal(t, "application/json", h.Get("Content-Type"))
				assert.Empty(t, h.Get("Authorization"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := tt.setupClient()
			client.client = &mockHTTPClient{
				response: tt.mockResponse,
				err:      tt.mockError,
			}
			req := tt.setupRequest()

			response, err := client.do(req)

			if tt.expectedError != nil {
				assert.Error(t, err)
				assert.Equal(t, tt.expectedError, err)
			} else {
				assert.NoError(t, err)
			}

			assert.Equal(t, tt.expectedResponse, response)

			if response != nil {
				assert.Equal(t, tt.expectedStatusCode, response.StatusCode)
			}

			if tt.checkHeaders != nil {
				tt.checkHeaders(t, req.Header)
			}
		})
	}
}

type mockHTTPClient struct {
	response *http.Response
	err      error
}

func (m *mockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return m.response, m.err
}
