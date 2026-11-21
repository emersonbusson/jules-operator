package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultJulesAPIBaseURL = "https://jules.googleapis.com/v1alpha"
	maxPromptCharacters    = 20_000
	maxTitleCharacters     = 200
	maxResponseBytes       = 8 << 20
	maxSingleActivityBytes = 32 << 20
	allowedSourceName      = "sources/github/emersonbusson/ramshared"
	defaultStartingBranch  = "main"
	unspecifiedAutomation  = "AUTOMATION_MODE_UNSPECIFIED"
	sessionsEndpoint       = "/sessions"
)

var errJulesResponseTooLarge = errors.New("resposta Jules excede o limite")

type julesClient struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

type julesHTTPError struct {
	StatusCode int
}

func (e *julesHTTPError) Error() string {
	return fmt.Sprintf("Jules returned HTTP %d", e.StatusCode)
}

func isJulesHTTPStatus(err error, status int) bool {
	var httpErr *julesHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == status
}

func newJulesClient(apiKey, baseURL string, httpClient *http.Client) (*julesClient, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("JULES_API_KEY não configurada")
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultJulesAPIBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Path != "" && parsed.Path != "/v1alpha" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("JULES_API_BASE_URL possui formato inválido")
	}
	local := parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost")
	official := parsed.Scheme == "https" && parsed.Hostname() == "jules.googleapis.com"
	if !local && !official {
		return nil, errors.New("JULES_API_BASE_URL deve usar HTTPS; localhost é permitido apenas para testes")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &julesClient{apiKey: apiKey, baseURL: strings.TrimRight(baseURL, "/"), http: httpClient}, nil
}

func (c *julesClient) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	return c.doWithResponseLimit(ctx, method, path, body, maxResponseBytes)
}

func (c *julesClient) doWithResponseLimit(
	ctx context.Context,
	method string,
	path string,
	body any,
	responseLimit int,
) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("x-goog-api-key", c.apiKey)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jules request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(responseLimit)+1))
	if err != nil {
		return nil, fmt.Errorf("read Jules response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &julesHTTPError{StatusCode: resp.StatusCode}
	}
	if len(data) > responseLimit {
		return nil, fmt.Errorf(
			"%w de %d bytes",
			errJulesResponseTooLarge,
			responseLimit,
		)
	}
	if len(data) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid(data) {
		return nil, errors.New("jules returned invalid JSON")
	}
	return json.RawMessage(data), nil
}

func validPrompt(value string) bool {
	characters := utf8.RuneCountInString(strings.TrimSpace(value))
	return characters > 0 && characters <= maxPromptCharacters
}

func validTitle(value string) bool {
	characters := utf8.RuneCountInString(strings.TrimSpace(value))
	return characters > 0 && characters <= maxTitleCharacters
}
