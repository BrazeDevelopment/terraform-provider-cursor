package publicapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.cursor.com"

	defaultHTTPTimeout = 30 * time.Second
	maxResponseBytes   = 10 << 20
	maxListPages       = 1000

	redactedSecret = "[redacted]"
)

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
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
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
	path, err := environmentPath(id)
	if err != nil {
		return nil, err
	}
	var out Environment
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateEnvironment(ctx context.Context, id string, req UpdateEnvironmentRequest) error {
	path, err := environmentPath(id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, path, nil, req, nil, http.StatusNoContent)
}

func (c *Client) DeleteEnvironment(ctx context.Context, id string) error {
	path, err := environmentPath(id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil, http.StatusOK)
}

// ListEnvironments walks every page. An environment that changes during the walk
// moves to the front of the list, so a later page can repeat one already returned;
// each id is returned once, in the order first seen.
func (c *Client) ListEnvironments(ctx context.Context) ([]Environment, error) {
	var all []Environment
	seen := map[string]struct{}{}
	err := paginate("environments", func(cursor string) (*string, error) {
		query := url.Values{"limit": {"100"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var out EnvironmentPage
		if err := c.do(ctx, http.MethodGet, "/v1/environments", query, nil, &out, http.StatusOK); err != nil {
			return nil, err
		}
		for _, env := range out.Items {
			if env.ID != "" {
				if _, dup := seen[env.ID]; dup {
					continue
				}
				seen[env.ID] = struct{}{}
			}
			all = append(all, env)
		}
		return out.NextCursor, nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

func (c *Client) GetActiveBuild(ctx context.Context, environmentID string) (*ActiveBuild, error) {
	path, err := environmentPath(environmentID)
	if err != nil {
		return nil, err
	}
	var out ActiveBuild
	if err := c.do(ctx, http.MethodGet, path+"/builds/active", nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetBuild(ctx context.Context, environmentID, buildID string) (*EnvironmentBuild, error) {
	path, err := environmentPath(environmentID)
	if err != nil {
		return nil, err
	}
	build, err := segment("build id", buildID)
	if err != nil {
		return nil, err
	}
	var out EnvironmentBuild
	if err := c.do(ctx, http.MethodGet, path+"/builds/"+build, nil, nil, &out, http.StatusOK); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListEnvironmentSecrets(ctx context.Context, environmentID string) ([]SecretVersion, error) {
	path, err := environmentPath(environmentID)
	if err != nil {
		return nil, err
	}
	return c.listSecrets(ctx, path+"/secrets")
}

func (c *Client) PutEnvironmentSecret(ctx context.Context, environmentID, name, versionID string, req SetSecretRequest) (*SecretVersion, int, error) {
	path, err := environmentSecretPath(environmentID, name)
	if err != nil {
		return nil, 0, err
	}
	return c.putSecret(ctx, path, versionID, req)
}

func (c *Client) DeleteEnvironmentSecret(ctx context.Context, environmentID, name, versionID string) error {
	path, err := environmentSecretPath(environmentID, name)
	if err != nil {
		return err
	}
	return c.deleteSecret(ctx, path, versionID)
}

func (c *Client) ListTeamSecrets(ctx context.Context) ([]SecretVersion, error) {
	return c.listSecrets(ctx, "/v1/team/secrets")
}

func (c *Client) PutTeamSecret(ctx context.Context, name, versionID string, req SetSecretRequest) (*SecretVersion, int, error) {
	path, err := teamSecretPath(name)
	if err != nil {
		return nil, 0, err
	}
	return c.putSecret(ctx, path, versionID, req)
}

func (c *Client) DeleteTeamSecret(ctx context.Context, name, versionID string) error {
	path, err := teamSecretPath(name)
	if err != nil {
		return err
	}
	return c.deleteSecret(ctx, path, versionID)
}

// listSecrets follows nextCursor. The secret lists are documented as unpaginated
// with nextCursor always null; a non-null cursor is followed with the cursor query
// parameter the other list endpoints use, so a list that starts paginating is read
// completely or fails, and is never cut at the first page.
func (c *Client) listSecrets(ctx context.Context, path string) ([]SecretVersion, error) {
	var all []SecretVersion
	seen := map[string]struct{}{}
	err := paginate("secrets", func(cursor string) (*string, error) {
		query := url.Values{}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var out SecretPage
		if err := c.do(ctx, http.MethodGet, path, query, nil, &out, http.StatusOK); err != nil {
			return nil, err
		}
		for _, version := range out.Items {
			if version.ID != "" {
				if _, dup := seen[version.ID]; dup {
					continue
				}
				seen[version.ID] = struct{}{}
			}
			all = append(all, version)
		}
		return out.NextCursor, nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

func (c *Client) putSecret(ctx context.Context, path, versionID string, req SetSecretRequest) (*SecretVersion, int, error) {
	query := url.Values{}
	if versionID != "" {
		query.Set("id", versionID)
	}
	var out SecretVersion
	status, err := c.doStatus(ctx, http.MethodPut, path, query, req, &out, http.StatusOK, http.StatusCreated)
	if err != nil {
		return nil, 0, scrubSecret(err, req.Value)
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

// paginate calls fetch with each cursor until the API stops returning one. A
// cursor the API already returned would loop forever, and stopping there would
// return a list that looks complete and is not, so it is an error.
func paginate(what string, fetch func(cursor string) (*string, error)) error {
	seen := map[string]struct{}{}
	cursor := ""
	for range maxListPages {
		next, err := fetch(cursor)
		if err != nil {
			return err
		}
		if next == nil || *next == "" {
			return nil
		}
		if _, dup := seen[*next]; dup {
			return fmt.Errorf("listing %s: the Cursor API returned the same pagination cursor twice, so the list cannot be read completely", what)
		}
		seen[*next] = struct{}{}
		cursor = *next
	}
	return fmt.Errorf("listing %s exceeded %d pages", what, maxListPages)
}

// segment escapes one path segment. An empty segment would turn a single-item
// path into its collection path, and "." or ".." can be rewritten on the way to
// the API, so none of them is sent.
func segment(kind, value string) (string, error) {
	if value == "" || value == "." || value == ".." {
		return "", fmt.Errorf("%s %q cannot be used in a Cursor API path", kind, value)
	}
	return url.PathEscape(value), nil
}

func environmentPath(id string) (string, error) {
	escaped, err := segment("environment id", id)
	if err != nil {
		return "", err
	}
	return "/v1/environments/" + escaped, nil
}

func environmentSecretPath(environmentID, name string) (string, error) {
	path, err := environmentPath(environmentID)
	if err != nil {
		return "", err
	}
	escaped, err := segment("secret name", name)
	if err != nil {
		return "", err
	}
	return path + "/secrets/" + escaped, nil
}

func teamSecretPath(name string) (string, error) {
	escaped, err := segment("secret name", name)
	if err != nil {
		return "", err
	}
	return "/v1/team/secrets/" + escaped, nil
}

// scrubSecret removes a secret value from the server-supplied text of an API
// error before it can reach a diagnostic or a log. The API documents that no
// response includes the value; this holds even if a response does. Other errors
// are built from the URL and fixed text and never carry the request body.
func scrubSecret(err error, value string) error {
	var apiErr *Error
	if value == "" || !errors.As(err, &apiErr) {
		return err
	}
	if !strings.Contains(apiErr.Code, value) && !strings.Contains(apiErr.Message, value) {
		return err
	}
	clean := *apiErr
	clean.Code = strings.ReplaceAll(clean.Code, value, redactedSecret)
	clean.Message = strings.ReplaceAll(clean.Message, value, redactedSecret)
	return &clean
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("reading Cursor API response: %w", err)
	}
	if !statusOK(method, resp.StatusCode, success) {
		return resp.StatusCode, decodeError(resp.StatusCode, raw, secretPath)
	}
	if len(raw) > maxResponseBytes {
		return resp.StatusCode, fmt.Errorf("Cursor API response to %s %s (HTTP %d) is larger than %d bytes and was not read", method, path, resp.StatusCode, maxResponseBytes)
	}
	if dest != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, dest); err != nil {
			if secretPath {
				return resp.StatusCode, fmt.Errorf("decoding Cursor API response (HTTP %d): the body is not the expected JSON", resp.StatusCode)
			}
			return resp.StatusCode, fmt.Errorf("decoding Cursor API response (HTTP %d): %w", resp.StatusCode, err)
		}
	}
	return resp.StatusCode, nil
}

// statusOK accepts the documented success codes. A write that returns any other
// 2xx has still taken effect, and reporting it as a failure would leave a created
// object untracked, so writes accept every 2xx. Reads stay strict: an unexpected
// status on a read must not be decoded as an empty result.
func statusOK(method string, status int, success []int) bool {
	if slices.Contains(success, status) {
		return true
	}
	if len(success) == 0 || method != http.MethodGet {
		return status >= 200 && status < 300
	}
	return false
}
