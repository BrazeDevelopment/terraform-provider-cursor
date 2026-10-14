package publicapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const defaultBaseURL = "https://api.cursor.com"

// Doer performs an HTTP request. The provider passes its retrying client.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client calls the Cursor public API at api.cursor.com.
type Client struct {
	baseURL   string
	apiKey    string
	userAgent string
	http      Doer
}

type Options struct {
	BaseURL   string
	APIKey    string
	UserAgent string
	HTTP      Doer
}

func New(opts Options) *Client {
	base := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	httpClient := opts.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:   base,
		apiKey:    strings.TrimSpace(opts.APIKey),
		userAgent: strings.TrimSpace(opts.UserAgent),
		http:      httpClient,
	}
}

func (c *Client) CreateEnvironment(ctx context.Context, req CreateEnvironmentRequest) (*Environment, error) {
	var out Environment
	if err := c.do(ctx, http.MethodPost, "/v1/environments", nil, req, &out, http.StatusCreated); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetEnvironment(ctx context.Context, id string) (*Environment, error) {
	var out Environment
	if err := c.do(ctx, http.MethodGet, "/v1/environments/"+url.PathEscape(id), nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateEnvironment(ctx context.Context, id string, req UpdateEnvironmentRequest) error {
	return c.do(ctx, http.MethodPatch, "/v1/environments/"+url.PathEscape(id), nil, req, nil, http.StatusNoContent)
}

func (c *Client) DeleteEnvironment(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/environments/"+url.PathEscape(id), nil, nil, nil, http.StatusOK)
}

func (c *Client) ListEnvironments(ctx context.Context) ([]Environment, error) {
	var all []Environment
	cursor := ""
	seen := map[string]struct{}{}
	for page := 0; page < 1000; page++ {
		query := url.Values{"limit": {"100"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var out EnvironmentPage
		if err := c.do(ctx, http.MethodGet, "/v1/environments", query, nil, &out, http.StatusOK); err != nil {
			return nil, err
		}
		all = append(all, out.Items...)
		if out.NextCursor == nil || *out.NextCursor == "" || *out.NextCursor == cursor {
			return all, nil
		}
		if _, ok := seen[*out.NextCursor]; ok {
			return all, nil
		}
		seen[*out.NextCursor] = struct{}{}
		cursor = *out.NextCursor
	}
	return nil, fmt.Errorf("listing environments exceeded 1000 pages")
}

func (c *Client) GetActiveBuild(ctx context.Context, environmentID string) (*ActiveBuild, error) {
	var out ActiveBuild
	path := "/v1/environments/" + url.PathEscape(environmentID) + "/builds/active"
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetBuild(ctx context.Context, environmentID, buildID string) (*EnvironmentBuild, error) {
	var out EnvironmentBuild
	path := "/v1/environments/" + url.PathEscape(environmentID) + "/builds/" + url.PathEscape(buildID)
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListEnvironmentSecrets(ctx context.Context, environmentID string) ([]SecretVersion, error) {
	var out SecretPage
	path := "/v1/environments/" + url.PathEscape(environmentID) + "/secrets"
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (c *Client) PutEnvironmentSecret(ctx context.Context, environmentID, name, versionID string, req SetSecretRequest) (*SecretVersion, int, error) {
	path := "/v1/environments/" + url.PathEscape(environmentID) + "/secrets/" + url.PathEscape(name)
	return c.putSecret(ctx, path, versionID, req)
}

func (c *Client) DeleteEnvironmentSecret(ctx context.Context, environmentID, name, versionID string) error {
	path := "/v1/environments/" + url.PathEscape(environmentID) + "/secrets/" + url.PathEscape(name)
	return c.deleteSecret(ctx, path, versionID)
}

func (c *Client) ListTeamSecrets(ctx context.Context) ([]SecretVersion, error) {
	var out SecretPage
	if err := c.do(ctx, http.MethodGet, "/v1/team/secrets", nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (c *Client) PutTeamSecret(ctx context.Context, name, versionID string, req SetSecretRequest) (*SecretVersion, int, error) {
	return c.putSecret(ctx, "/v1/team/secrets/"+url.PathEscape(name), versionID, req)
}

func (c *Client) DeleteTeamSecret(ctx context.Context, name, versionID string) error {
	return c.deleteSecret(ctx, "/v1/team/secrets/"+url.PathEscape(name), versionID)
}

func (c *Client) putSecret(ctx context.Context, path, versionID string, req SetSecretRequest) (*SecretVersion, int, error) {
	query := url.Values{}
	if versionID != "" {
		query.Set("id", versionID)
	}
	var out SecretVersion
	status, err := c.doStatus(ctx, http.MethodPut, path, query, req, &out, http.StatusOK, http.StatusCreated)
	if err != nil {
		return nil, 0, err
	}
	return &out, status, nil
}

func (c *Client) deleteSecret(ctx context.Context, path, versionID string) error {
	query := url.Values{}
	if versionID != "" {
		query.Set("id", versionID)
	}
	return c.do(ctx, http.MethodDelete, path, query, nil, nil, http.StatusOK)
}

func (c *Client) APIKeyInfo(ctx context.Context) (*APIKeyInfo, error) {
	var out APIKeyInfo
	if err := c.do(ctx, http.MethodGet, "/v1/me", nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	var out ModelPage
	if err := c.do(ctx, http.MethodGet, "/v1/models", nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (c *Client) RegisterPool(ctx context.Context, req RegisterPoolRequest) error {
	var out struct {
		Registered bool `json:"registered"`
	}
	if err := c.do(ctx, http.MethodPost, "/v0/private-workers/pools", nil, req, &out, http.StatusOK, http.StatusCreated); err != nil {
		return err
	}
	if !out.Registered {
		return fmt.Errorf("pool register returned registered=false")
	}
	return nil
}

func (c *Client) DeregisterPool(ctx context.Context, scope, poolName, repoOwner, repoName string) error {
	query := url.Values{
		"scope":     {scope},
		"pool_name": {poolName},
	}
	if repoOwner != "" || repoName != "" {
		query.Set("repo_owner", repoOwner)
		query.Set("repo_name", repoName)
	}
	return c.do(ctx, http.MethodDelete, "/v0/private-workers/pools", query, nil, nil, http.StatusOK, http.StatusNoContent)
}

func (c *Client) ListPools(ctx context.Context, scope string) ([]Pool, error) {
	query := url.Values{}
	if scope != "" {
		query.Set("scope", scope)
	}
	var out PoolPage
	if err := c.do(ctx, http.MethodGet, "/v0/private-workers/pools", query, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return out.Pools, nil
}

func (c *Client) WorkerSummary(ctx context.Context) (*WorkerSummary, error) {
	var out WorkerSummary
	if err := c.do(ctx, http.MethodGet, "/v0/private-workers/summary", nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, payload any, dest any, success ...int) error {
	_, err := c.doStatus(ctx, method, path, query, payload, dest, success...)
	return err
}

func (c *Client) doStatus(ctx context.Context, method, path string, query url.Values, payload any, dest any, success ...int) (int, error) {
	if c == nil || c.http == nil {
		return 0, fmt.Errorf("Cursor API client is not configured")
	}
	if strings.TrimSpace(c.apiKey) == "" {
		return 0, fmt.Errorf("api_key is required; set the provider api_key attribute or CURSOR_API_KEY")
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, fmt.Errorf("encoding request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.apiKey+":")))
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("calling Cursor API: %w", err)
	}
	defer resp.Body.Close()
	// Secret endpoints never contribute their bodies to logs or wrapped errors.
	secretPath := strings.Contains(path, "/secrets")
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("reading Cursor API response: %w", err)
	}
	if !statusOK(resp.StatusCode, success) {
		return resp.StatusCode, decodeError(resp.StatusCode, raw, secretPath)
	}
	if dest != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, dest); err != nil {
			return resp.StatusCode, fmt.Errorf("decoding Cursor API response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func statusOK(status int, success []int) bool {
	if len(success) == 0 {
		return status >= 200 && status < 300
	}
	for _, code := range success {
		if status == code {
			return true
		}
	}
	return false
}
