package publicapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

type recordedRequest struct {
	Method      string
	EscapedPath string
	RawQuery    string
	Body        string
	Header      http.Header
}

type recorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *recorder) add(req *http.Request) recordedRequest {
	raw, _ := io.ReadAll(req.Body)
	rec := recordedRequest{Method: req.Method, EscapedPath: req.URL.EscapedPath(), RawQuery: req.URL.RawQuery, Body: string(raw), Header: req.Header.Clone()}
	r.mu.Lock()
	r.requests = append(r.requests, rec)
	r.mu.Unlock()
	return rec
}

func (r *recorder) all() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRequest(nil), r.requests...)
}

func testClient(t *testing.T, handler func(w http.ResponseWriter, rec recordedRequest)) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, rec.add(r))
	}))
	t.Cleanup(server.Close)
	return New(Options{BaseURL: server.URL + "/", APIKey: " key_test ", UserAgent: "terraform-provider-cursor/test+braze"}), rec
}

func TestErrorHelpersSeeThroughWrappedErrors(t *testing.T) {
	apiErr := &Error{Status: 404, Code: "environment_not_found", Message: "gone"}
	wrapped := fmt.Errorf("reading environment: %w", apiErr)
	if !IsNotFound(apiErr) || !IsNotFound(wrapped) {
		t.Fatal("IsNotFound must match a direct and a wrapped 404")
	}
	if !IsStatus(wrapped, 404) || IsStatus(wrapped, 409) {
		t.Fatal("IsStatus must compare the wrapped status")
	}
	if !HasCode(wrapped, "environment_not_found") || HasCode(wrapped, "secret_not_found") {
		t.Fatal("HasCode must compare the wrapped code")
	}
	for _, err := range []error{nil, errors.New("HTTP 404"), context.Canceled} {
		if IsNotFound(err) || IsStatus(err, 404) || HasCode(err, "environment_not_found") {
			t.Fatalf("%v is not an API error", err)
		}
	}
}

func TestDecodeErrorBodyShapes(t *testing.T) {
	cases := []struct {
		name, body    string
		redact        bool
		code, message string
	}{
		{name: "nested object", body: `{"error":{"code":"environment_not_found","message":"missing"}}`, code: "environment_not_found", message: "missing"},
		{name: "authentication error", body: `{"code":"error","message":"Invalid User API Key"}`, code: "error", message: "Invalid User API Key"},
		{name: "error string", body: `{"error":"Not Found"}`, message: "Not Found"},
		{name: "error string redacted", body: `{"error":"Not Found"}`, redact: true, message: "Not Found"},
		{name: "null error with top-level message", body: `{"error":null,"message":"down"}`, message: "down"},
		{name: "non-string code", body: `{"error":{"code":404,"message":"missing"}}`, message: "missing"},
		{name: "plain text", body: "upstream connect error", message: "upstream connect error"},
		{name: "plain text redacted", body: "upstream said super-secret", redact: true, message: "request failed"},
		{name: "empty redacted", body: "", redact: true, message: "request failed"},
		{name: "json array", body: `["x"]`, message: `["x"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var apiErr *Error
			if !errors.As(decodeError(500, []byte(tc.body), tc.redact), &apiErr) {
				t.Fatal("decodeError must return *Error")
			}
			if apiErr.Status != 500 || apiErr.Code != tc.code || apiErr.Message != tc.message {
				t.Fatalf("got status=%d code=%q message=%q", apiErr.Status, apiErr.Code, apiErr.Message)
			}
		})
	}
}

func TestDecodeErrorTextIsBoundedValidUTF8(t *testing.T) {
	// 511 ASCII bytes followed by a three-byte character: a cut at 512 bytes lands inside it.
	raw := strings.Repeat("a", maxRawErrorBytes-1) + strings.Repeat("\u20ac", 50)
	var apiErr *Error
	errors.As(decodeError(502, []byte(raw), false), &apiErr)
	if !utf8.ValidString(apiErr.Message) || len(apiErr.Message) > maxRawErrorBytes || len(apiErr.Message) < maxRawErrorBytes-1 {
		t.Fatalf("raw message valid=%t len=%d", utf8.ValidString(apiErr.Message), len(apiErr.Message))
	}

	long := strings.Repeat("\u20ac", maxErrorMessageBytes)
	errors.As(decodeError(400, []byte(`{"error":{"code":"validation_error","message":"`+long+`"}}`), false), &apiErr)
	if !utf8.ValidString(apiErr.Message) || len(apiErr.Message) > maxErrorMessageBytes || apiErr.Message == "" {
		t.Fatalf("structured message valid=%t len=%d", utf8.ValidString(apiErr.Message), len(apiErr.Message))
	}

	errors.As(decodeError(500, []byte("bad \xff\xfe bytes"), false), &apiErr)
	if !utf8.ValidString(apiErr.Message) || !strings.Contains(apiErr.Message, "bytes") {
		t.Fatalf("invalid bytes were kept: %q", apiErr.Message)
	}
}

func TestNameConflictErrorNamesTheExistingEnvironment(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"environment_name_conflict","message":"name taken","environmentId":"8f14e45f-ceea-4e6b-9c3a-1d2e3f4a5b6c"}}`)
	})
	_, err := client.CreateEnvironment(context.Background(), CreateEnvironmentRequest{Owner: "team", Name: "web", Repos: []EnvironmentRepo{}, EnvironmentJSON: "{}"})
	if !IsStatus(err, 409) || !HasCode(err, "environment_name_conflict") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "Existing environment id: 8f14e45f-ceea-4e6b-9c3a-1d2e3f4a5b6c.") {
		t.Fatalf("the error does not name the existing environment: %v", err)
	}
}

func TestRequestsCarryBasicAuthAndUserAgent(t *testing.T) {
	client, rec := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		_, _ = io.WriteString(w, `{"apiKeyName":"terraform","createdAt":"2026-01-01T00:00:00Z"}`)
	})
	if _, err := client.APIKeyInfo(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := rec.all()[0]
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("key_test:"))
	if got.Header.Get("Authorization") != want {
		t.Fatalf("Authorization = %q, want the trimmed key as username with an empty password", got.Header.Get("Authorization"))
	}
	if got.Header.Get("User-Agent") != "terraform-provider-cursor/test+braze" || got.Header.Get("Accept") != "application/json" {
		t.Fatalf("headers = %v", got.Header)
	}
	if got.EscapedPath != "/v1/me" {
		t.Fatalf("a trailing slash on the base URL changed the path: %q", got.EscapedPath)
	}
}

func TestMissingKeyAndNilClientFailBeforeAnyRequest(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.APIKeyInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("nil client err = %v", err)
	}
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rec.add(r) }))
	defer server.Close()
	if _, err := New(Options{BaseURL: server.URL, APIKey: "   "}).ListModels(context.Background()); err == nil || !strings.Contains(err.Error(), "api_key is required") {
		t.Fatalf("blank key err = %v", err)
	}
	if len(rec.all()) != 0 {
		t.Fatal("a request was sent without an API key")
	}
}

func TestDefaultHTTPClientHasATimeout(t *testing.T) {
	httpClient, ok := New(Options{APIKey: "key_test"}).http.(*http.Client)
	if !ok || httpClient.Timeout != defaultHTTPTimeout || httpClient.Timeout <= 0 {
		t.Fatalf("default HTTP client = %#v", New(Options{}).http)
	}
}

func TestListEnvironmentsWalksEveryPageOnce(t *testing.T) {
	item := func(id string) string {
		return `{"id":"` + id + `","owner":"team","repos":[],"createdAt":"t","updatedAt":"t"}`
	}
	client, rec := testClient(t, func(w http.ResponseWriter, r recordedRequest) {
		switch {
		case strings.Contains(r.RawQuery, "cursor=p2"):
			// env-2 moved while the walk was in flight and is repeated on this page.
			_, _ = io.WriteString(w, `{"items":[`+item("env-2")+`,`+item("env-3")+`],"nextCursor":"p3"}`)
		case strings.Contains(r.RawQuery, "cursor=p3"):
			_, _ = io.WriteString(w, `{"items":[]}`)
		default:
			_, _ = io.WriteString(w, `{"items":[`+item("env-1")+`,`+item("env-2")+`],"nextCursor":"p2"}`)
		}
	})
	envs, err := client.ListEnvironments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, env := range envs {
		ids = append(ids, env.ID)
	}
	if strings.Join(ids, ",") != "env-1,env-2,env-3" {
		t.Fatalf("ids = %v, want each environment once in first-seen order", ids)
	}
	requests := rec.all()
	if len(requests) != 3 || !strings.Contains(requests[0].RawQuery, "limit=100") || strings.Contains(requests[0].RawQuery, "cursor") {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestListEnvironmentsFailsOnARepeatedCursor(t *testing.T) {
	for _, cursors := range [][]string{{"same", "same"}, {"a", "b", "a"}} {
		var call int
		client, rec := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
			next := cursors[min(call, len(cursors)-1)]
			call++
			_, _ = io.WriteString(w, `{"items":[{"id":"env-`+strconv.Itoa(call)+`","owner":"team","repos":[],"createdAt":"t","updatedAt":"t"}],"nextCursor":"`+next+`"}`)
		})
		envs, err := client.ListEnvironments(context.Background())
		if err == nil || !strings.Contains(err.Error(), "same pagination cursor twice") {
			t.Fatalf("cursors %v: err = %v", cursors, err)
		}
		if envs != nil {
			t.Fatalf("cursors %v: a partial list was returned: %v", cursors, envs)
		}
		if got := len(rec.all()); got != len(cursors) {
			t.Fatalf("cursors %v: %d requests, want %d", cursors, got, len(cursors))
		}
	}
}

func TestListEnvironmentsStopsAtThePageLimit(t *testing.T) {
	var call int
	client, rec := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		call++
		_, _ = io.WriteString(w, `{"items":[],"nextCursor":"c`+strconv.Itoa(call)+`"}`)
	})
	if _, err := client.ListEnvironments(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeded 1000 pages") {
		t.Fatalf("err = %v", err)
	}
	if got := len(rec.all()); got != maxListPages {
		t.Fatalf("%d requests, want %d", got, maxListPages)
	}
}

func TestListStopsWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, rec := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		_, _ = io.WriteString(w, `{"items":[],"nextCursor":"next"}`)
		cancel()
	})
	_, err := client.ListEnvironments(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := len(rec.all()); got != 1 {
		t.Fatalf("%d requests after cancellation, want 1", got)
	}
}

func TestSecretListsFollowNextCursor(t *testing.T) {
	version := func(id, name string) string {
		return `{"id":"` + id + `","name":"` + name + `","type":"runtime_secret","repos":[],"createdAt":"t"}`
	}
	lists := map[string]func(*Client) ([]SecretVersion, error){
		"/v1/team/secrets": func(c *Client) ([]SecretVersion, error) { return c.ListTeamSecrets(context.Background()) },
		"/v1/environments/env-1/secrets": func(c *Client) ([]SecretVersion, error) {
			return c.ListEnvironmentSecrets(context.Background(), "env-1")
		},
	}
	for path, list := range lists {
		t.Run("paginated "+path, func(t *testing.T) {
			client, rec := testClient(t, func(w http.ResponseWriter, r recordedRequest) {
				if r.RawQuery == "cursor=p2" {
					// The first version is repeated; a duplicate would make its name look ambiguous.
					_, _ = io.WriteString(w, `{"items":[`+version("secv_a", "A")+`,`+version("secv_b", "B")+`],"nextCursor":null}`)
					return
				}
				_, _ = io.WriteString(w, `{"items":[`+version("secv_a", "A")+`],"nextCursor":"p2"}`)
			})
			versions, err := list(client)
			if err != nil {
				t.Fatal(err)
			}
			if len(versions) != 2 || versions[0].ID != "secv_a" || versions[1].ID != "secv_b" {
				t.Fatalf("versions = %#v", versions)
			}
			requests := rec.all()
			if len(requests) != 2 || requests[0].EscapedPath != path || requests[0].RawQuery != "" || requests[1].RawQuery != "cursor=p2" {
				t.Fatalf("requests = %#v", requests)
			}
		})
		t.Run("unpaginated "+path, func(t *testing.T) {
			client, rec := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
				_, _ = io.WriteString(w, `{"items":[`+version("secv_a", "A")+`],"nextCursor":null}`)
			})
			versions, err := list(client)
			if err != nil || len(versions) != 1 || len(rec.all()) != 1 {
				t.Fatalf("versions=%v err=%v requests=%d", versions, err, len(rec.all()))
			}
		})
		t.Run("cursor ignored by the server "+path, func(t *testing.T) {
			client, rec := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
				_, _ = io.WriteString(w, `{"items":[`+version("secv_a", "A")+`],"nextCursor":"p2"}`)
			})
			versions, err := list(client)
			if err == nil || versions != nil || !strings.Contains(err.Error(), "same pagination cursor twice") {
				t.Fatalf("a truncated list must be an error: versions=%v err=%v", versions, err)
			}
			if len(rec.all()) != 2 {
				t.Fatalf("%d requests, want 2", len(rec.all()))
			}
		})
	}
}

func TestPathSegmentsAreEscaped(t *testing.T) {
	client, rec := testClient(t, func(w http.ResponseWriter, r recordedRequest) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.EscapedPath, "/secrets"):
			_, _ = io.WriteString(w, `{"items":[],"nextCursor":null}`)
		case r.Method == http.MethodGet && strings.Contains(r.EscapedPath, "/builds/active"):
			_, _ = io.WriteString(w, `{"type":"universal_image"}`)
		case r.Method == http.MethodGet && strings.Contains(r.EscapedPath, "/builds/"):
			_, _ = io.WriteString(w, `{"id":"b","status":"SUCCEEDED"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"id":"x","owner":"team","repos":[],"createdAt":"t","updatedAt":"t"}`)
		case r.Method == http.MethodPut:
			_, _ = io.WriteString(w, `{"id":"secv_a","name":"n","type":"runtime_secret","repos":[]}`)
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	ctx := context.Background()
	const id = "a/b ?#%"
	const escaped = "a%2Fb%20%3F%23%25"
	name := "X"
	calls := []struct {
		want string
		call func() error
	}{
		{"GET /v1/environments/" + escaped, func() error { _, err := client.GetEnvironment(ctx, id); return err }},
		{"PATCH /v1/environments/" + escaped, func() error { return client.UpdateEnvironment(ctx, id, UpdateEnvironmentRequest{Name: &name}) }},
		{"DELETE /v1/environments/" + escaped, func() error { return client.DeleteEnvironment(ctx, id) }},
		{"GET /v1/environments/" + escaped + "/builds/active", func() error { _, err := client.GetActiveBuild(ctx, id); return err }},
		{"GET /v1/environments/" + escaped + "/builds/" + escaped, func() error { _, err := client.GetBuild(ctx, id, id); return err }},
		{"GET /v1/environments/" + escaped + "/secrets", func() error { _, err := client.ListEnvironmentSecrets(ctx, id); return err }},
		{"PUT /v1/environments/" + escaped + "/secrets/" + escaped, func() error {
			_, _, err := client.PutEnvironmentSecret(ctx, id, id, "", SetSecretRequest{Value: "v", Repos: []string{}})
			return err
		}},
		{"DELETE /v1/environments/" + escaped + "/secrets/" + escaped, func() error { return client.DeleteEnvironmentSecret(ctx, id, id, "") }},
		{"PUT /v1/team/secrets/" + escaped, func() error {
			_, _, err := client.PutTeamSecret(ctx, id, "", SetSecretRequest{Value: "v", Repos: []string{}})
			return err
		}},
		{"DELETE /v1/team/secrets/" + escaped, func() error { return client.DeleteTeamSecret(ctx, id, "") }},
	}
	for i, tc := range calls {
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.want, err)
		}
		got := rec.all()[i]
		if got.Method+" "+got.EscapedPath != tc.want {
			t.Errorf("request = %s %s, want %s", got.Method, got.EscapedPath, tc.want)
		}
	}
}

func TestEmptyAndDotPathSegmentsAreNeverSent(t *testing.T) {
	client, rec := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		_, _ = io.WriteString(w, `{"items":[{"id":"other","owner":"team","repos":[],"createdAt":"t","updatedAt":"t"}]}`)
	})
	ctx := context.Background()
	name := "X"
	for _, bad := range []string{"", ".", ".."} {
		calls := map[string]func() error{
			"GetEnvironment":         func() error { _, err := client.GetEnvironment(ctx, bad); return err },
			"UpdateEnvironment":      func() error { return client.UpdateEnvironment(ctx, bad, UpdateEnvironmentRequest{Name: &name}) },
			"DeleteEnvironment":      func() error { return client.DeleteEnvironment(ctx, bad) },
			"GetActiveBuild":         func() error { _, err := client.GetActiveBuild(ctx, bad); return err },
			"GetBuild environment":   func() error { _, err := client.GetBuild(ctx, bad, "bld"); return err },
			"GetBuild build":         func() error { _, err := client.GetBuild(ctx, "env", bad); return err },
			"ListEnvironmentSecrets": func() error { _, err := client.ListEnvironmentSecrets(ctx, bad); return err },
			"PutEnvironmentSecret environment": func() error {
				_, _, err := client.PutEnvironmentSecret(ctx, bad, "N", "", SetSecretRequest{Value: "v"})
				return err
			},
			"PutEnvironmentSecret name": func() error {
				_, _, err := client.PutEnvironmentSecret(ctx, "env", bad, "", SetSecretRequest{Value: "v"})
				return err
			},
			"DeleteEnvironmentSecret name": func() error { return client.DeleteEnvironmentSecret(ctx, "env", bad, "") },
			"PutTeamSecret": func() error {
				_, _, err := client.PutTeamSecret(ctx, bad, "", SetSecretRequest{Value: "v"})
				return err
			},
			"DeleteTeamSecret": func() error { return client.DeleteTeamSecret(ctx, bad, "") },
		}
		for label, call := range calls {
			if err := call(); err == nil || !strings.Contains(err.Error(), "cannot be used in a Cursor API path") {
				t.Errorf("%s(%q) err = %v", label, bad, err)
			}
		}
	}
	if got := len(rec.all()); got != 0 {
		t.Fatalf("%d requests were sent for unusable ids: %#v", got, rec.all())
	}
}

func TestWritesAcceptAny2xxAndReadsStayStrict(t *testing.T) {
	const env = `{"id":"env-1","owner":"team","name":"web","repos":[],"createdAt":"t","updatedAt":"t"}`
	client, _ := testClient(t, func(w http.ResponseWriter, r recordedRequest) {
		switch r.Method {
		case http.MethodPost:
			_, _ = io.WriteString(w, env)
		case http.MethodPatch:
			_, _ = io.WriteString(w, env)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPut:
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"name":"N","type":"runtime_secret","repos":[]}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ctx := context.Background()
	created, err := client.CreateEnvironment(ctx, CreateEnvironmentRequest{Owner: "team", Name: "web", Repos: []EnvironmentRepo{}, EnvironmentJSON: "{}"})
	if err != nil || created.ID != "env-1" {
		t.Fatalf("a create answered with 200 must not be reported as failed: %v %v", created, err)
	}
	name := "web"
	if err := client.UpdateEnvironment(ctx, "env-1", UpdateEnvironmentRequest{Name: &name}); err != nil {
		t.Fatalf("update answered with 200: %v", err)
	}
	if err := client.DeleteEnvironment(ctx, "env-1"); err != nil {
		t.Fatalf("delete answered with 204: %v", err)
	}
	if _, status, err := client.PutTeamSecret(ctx, "N", "", SetSecretRequest{Value: "v", Repos: []string{}}); err != nil || status != http.StatusAccepted {
		t.Fatalf("put answered with 202: status=%d err=%v", status, err)
	}
	if _, err := client.GetEnvironment(ctx, "env-1"); !IsStatus(err, http.StatusNoContent) {
		t.Fatalf("a read answered with 204 must fail, not decode as empty: %v", err)
	}
	if versions, err := client.ListTeamSecrets(ctx); !IsStatus(err, http.StatusNoContent) || versions != nil {
		t.Fatalf("a list answered with 204 must fail, not read as an empty list: %v %v", versions, err)
	}
}

func TestOversizedResponses(t *testing.T) {
	big := strings.Repeat("x", maxResponseBytes)
	client, _ := testClient(t, func(w http.ResponseWriter, r recordedRequest) {
		if strings.HasSuffix(r.EscapedPath, "/missing") {
			w.WriteHeader(http.StatusNotFound)
		}
		_, _ = io.WriteString(w, `{"id":"env-1","owner":"team","environmentJson":"`+big+`"}`)
	})
	ctx := context.Background()
	_, err := client.GetEnvironment(ctx, "env-1")
	if err == nil || !strings.Contains(err.Error(), "larger than") || strings.Contains(err.Error(), "unexpected end of JSON") {
		t.Fatalf("oversized success body: %.200v", err)
	}
	if len(err.Error()) > 1024 {
		t.Fatalf("the error copies the body: %d bytes", len(err.Error()))
	}
	_, err = client.GetEnvironment(ctx, "missing")
	if !IsNotFound(err) {
		t.Fatalf("an oversized error body must keep its status: %.200v", err)
	}
	if len(err.Error()) > 1024 {
		t.Fatalf("the error copies the body: %d bytes", len(err.Error()))
	}
}

func TestResponseUnderTheSizeLimitIsRead(t *testing.T) {
	// Larger than the previous 1 MiB limit, which cut the body and failed to decode.
	big := strings.Repeat("x", 2<<20)
	client, _ := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		_, _ = io.WriteString(w, `{"id":"env-1","owner":"team","environmentJson":"`+big+`"}`)
	})
	env, err := client.GetEnvironment(context.Background(), "env-1")
	if err != nil || len(env.EnvironmentJSON) != len(big) {
		t.Fatalf("err = %.200v", err)
	}
}

func TestSecretValueNeverReachesAnError(t *testing.T) {
	const secret = "super-secret-value"
	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"structured message echoes the value", 400, `{"error":{"code":"validation_error","message":"value super-secret-value is not allowed"}}`, []string{"validation_error", redactedSecret}},
		{"code echoes the value", 400, `{"error":{"code":"super-secret-value","message":"no"}}`, []string{redactedSecret}},
		{"top-level message echoes the value", 401, `{"code":"error","message":"bad super-secret-value"}`, []string{redactedSecret}},
		{"error string echoes the value", 500, `{"error":"echo super-secret-value"}`, []string{redactedSecret}},
		{"plain body echoes the value", 502, `upstream echo super-secret-value`, []string{"request failed"}},
		{"unexpected json echoes the value", 500, `{"value":"super-secret-value"}`, []string{"request failed"}},
		{"undecodable success body echoes the value", 200, `super-secret-value`, []string{"not the expected JSON"}},
		{"mistyped success body echoes the value", 200, `{"name":{"super-secret-value":1}}`, []string{"not the expected JSON"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, rec := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			puts := map[string]func() error{
				"team": func() error {
					_, _, err := client.PutTeamSecret(context.Background(), "NPM_TOKEN", "", SetSecretRequest{Value: secret, Repos: []string{}})
					return err
				},
				"environment": func() error {
					_, _, err := client.PutEnvironmentSecret(context.Background(), "env-1", "NPM_TOKEN", "secv_a", SetSecretRequest{Value: secret, Repos: []string{}})
					return err
				},
			}
			for scope, put := range puts {
				err := put()
				if err == nil {
					t.Fatalf("%s: expected an error", scope)
				}
				text := fmt.Sprintf("%v %+v %#v", err, err, err)
				if strings.Contains(text, secret) {
					t.Fatalf("%s: the secret value leaked into the error: %s", scope, text)
				}
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%s: error %q does not contain %q", scope, err, want)
					}
				}
				if tc.status != 200 && !IsStatus(err, tc.status) {
					t.Errorf("%s: status was lost: %v", scope, err)
				}
			}
			for _, r := range rec.all() {
				if !strings.Contains(r.Body, secret) {
					t.Fatalf("the request body did not carry the value: %s", r.Body)
				}
			}
		})
	}

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	_, _, err := New(Options{BaseURL: closed.URL, APIKey: "key_test"}).PutTeamSecret(context.Background(), "NPM_TOKEN", "", SetSecretRequest{Value: secret, Repos: []string{}})
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "key_test") {
		t.Fatalf("transport error = %v", err)
	}
}

func TestSecretListAndDeleteErrorsNeverCopyTheBody(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, _ recordedRequest) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `<html>gateway dump: NPM_TOKEN=super-secret-value</html>`)
	})
	ctx := context.Background()
	errs := []error{client.DeleteTeamSecret(ctx, "NPM_TOKEN", ""), client.DeleteEnvironmentSecret(ctx, "env-1", "NPM_TOKEN", "")}
	_, err := client.ListTeamSecrets(ctx)
	errs = append(errs, err)
	_, err = client.ListEnvironmentSecrets(ctx, "env-1")
	errs = append(errs, err)
	for _, err := range errs {
		if !IsStatus(err, http.StatusBadGateway) || strings.Contains(err.Error(), "super-secret-value") || strings.Contains(err.Error(), "gateway dump") {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestOptionalAndNullFieldsDecode(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r recordedRequest) {
		switch r.EscapedPath {
		case "/v1/environments/env-1":
			_, _ = io.WriteString(w, `{"id":"env-1","owner":"team","name":null,"repos":null,"repoFile":null,"environmentJson":null,"versionId":null,"createdAt":"t","updatedAt":"t","futureField":{"a":1}}`)
		case "/v1/me":
			_, _ = io.WriteString(w, `{"apiKeyName":"terraform","createdAt":"t","userId":null,"userEmail":null}`)
		case "/v1/team/secrets":
			_, _ = io.WriteString(w, `{"items":null,"nextCursor":null}`)
		case "/v1/models":
			_, _ = io.WriteString(w, `{"items":[{"id":"m","displayName":"M","description":null,"aliases":null,"parameters":null,"variants":[{"params":[],"displayName":"V"}]}]}`)
		case "/v0/private-workers/summary":
			_, _ = io.WriteString(w, `{"teamSummary":null}`)
		case "/v0/private-workers/pools":
			_, _ = io.WriteString(w, `{"pools":null}`)
		case "/v1/environments/env-1/builds/active":
			_, _ = io.WriteString(w, `{"type":"universal_image"}`)
		}
	})
	ctx := context.Background()
	env, err := client.GetEnvironment(ctx, "env-1")
	if err != nil || env.Name != "" || env.Repos != nil || env.RepoFile != nil || env.EnvironmentJSON != "" || env.VersionID != "" {
		t.Fatalf("environment = %#v err = %v", env, err)
	}
	info, err := client.APIKeyInfo(ctx)
	if err != nil || info.UserID != nil || info.UserEmail != "" || info.APIKeyName != "terraform" {
		t.Fatalf("info = %#v err = %v", info, err)
	}
	if versions, err := client.ListTeamSecrets(ctx); err != nil || len(versions) != 0 {
		t.Fatalf("versions = %v err = %v", versions, err)
	}
	models, err := client.ListModels(ctx)
	if err != nil || len(models) != 1 || models[0].Aliases != nil || models[0].Parameters != nil || len(models[0].Variants) != 1 || models[0].Variants[0].IsDefault {
		t.Fatalf("models = %#v err = %v", models, err)
	}
	summary, err := client.WorkerSummary(ctx)
	if err != nil || summary.TeamSummary != nil || summary.UserSummary != nil {
		t.Fatalf("summary = %#v err = %v", summary, err)
	}
	if pools, err := client.ListPools(ctx, "all"); err != nil || len(pools) != 0 {
		t.Fatalf("pools = %v err = %v", pools, err)
	}
	active, err := client.GetActiveBuild(ctx, "env-1")
	if err != nil || active.Type != "universal_image" || active.BuildID != "" {
		t.Fatalf("active = %#v err = %v", active, err)
	}
}

// The client must never log: request and response bodies on the secret endpoints
// carry secret values. Errors are the only output, and they are scrubbed.
func TestPackageDoesNotImportALogger(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			if path == "log" || path == "log/slog" || strings.Contains(path, "tflog") || strings.Contains(path, "hclog") || path == "net/http/httputil" {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
}

func TestRepoSpellingsWithASchemeAndUser(t *testing.T) {
	for _, in := range []string{
		"ssh://git@github.com/acme/api.git",
		"SSH://git@GitHub.com/Acme/API",
		"https://token@github.com/acme/api",
		"HTTPS://github.com/acme/api/",
	} {
		if got := CanonicalRepo(in); got != "github.com/acme/api" {
			t.Errorf("CanonicalRepo(%q) = %q, want github.com/acme/api", in, got)
		}
		if got := RepoOwner(in); !strings.EqualFold(got, "acme") {
			t.Errorf("RepoOwner(%q) = %q, want acme", in, got)
		}
	}
	if got := RepoOwner("ssh://git@localhost/acme/api"); got != "acme" {
		t.Errorf("RepoOwner with a user on localhost = %q, want acme", got)
	}
}
