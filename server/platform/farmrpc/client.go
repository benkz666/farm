package farmrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Client sends a routed command to the Farm that owns it.
type Client interface {
	Execute(ctx context.Context, farmID string, request CommandRequest) (CommandResponse, error)
}

// HTTPClient implements Client over the internal HTTP JSON endpoint.
type HTTPClient struct {
	endpoints map[string]string
	token     string
	client    *http.Client
}

// NewHTTPClient constructs a client from physical farm IDs to base URLs.
func NewHTTPClient(endpoints map[string]string, token string) *HTTPClient {
	copied := make(map[string]string, len(endpoints))
	for farmID, endpoint := range endpoints {
		copied[farmID] = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	}
	return &HTTPClient{
		endpoints: copied,
		token:     token,
		client:    &http.Client{Timeout: 5 * time.Second},
	}
}

// Execute forwards exactly one client-authorized command to its Farm.
func (c *HTTPClient) Execute(ctx context.Context, farmID string, command CommandRequest) (CommandResponse, error) {
	if c == nil {
		return CommandResponse{}, fmt.Errorf("farmrpc: HTTP client is nil")
	}
	endpoint := c.endpoints[farmID]
	if endpoint == "" {
		return CommandResponse{}, fmt.Errorf("farmrpc: no endpoint configured for %q", farmID)
	}
	body, err := json.Marshal(command)
	if err != nil {
		return CommandResponse{}, fmt.Errorf("farmrpc: encode command: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+commandPath, bytes.NewReader(body))
	if err != nil {
		return CommandResponse{}, fmt.Errorf("farmrpc: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")

	response, err := c.client.Do(request)
	if err != nil {
		return CommandResponse{}, fmt.Errorf("farmrpc: execute command: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return CommandResponse{}, fmt.Errorf("farmrpc: command returned HTTP %d", response.StatusCode)
	}
	var result CommandResponse
	if err := decodeJSON(response.Body, &result); err != nil {
		return CommandResponse{}, fmt.Errorf("farmrpc: decode response: %w", err)
	}
	return result, nil
}
