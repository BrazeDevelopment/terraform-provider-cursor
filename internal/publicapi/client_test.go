package publicapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCanonicalRepoAndOwner(t *testing.T) {
	cases := []struct {
		in, repo, owner string
	}{
		{"acme/api", "github.com/acme/api", "acme"},
		{"https://github.com/acme/api.git", "github.com/acme/api", "acme"},
		{"git@github.com:acme/api.git", "github.com/acme/api", "acme"},
		{"gitlab.com/group/subgroup/project", "gitlab.com/group/subgroup/project", "group"},
		{"example-org", "github.com/example-org", "example-org"},
	}
	for _, tt := range cases {
		if got := CanonicalRepo(tt.in); got != tt.repo {
			t.Errorf("CanonicalRepo(%q) = %q, want %q", tt.in, got, tt.repo)
		}
		if got := RepoOwner(tt.in); got != tt.owner {
			t.Errorf("RepoOwner(%q) = %q, want %q", tt.in, got, tt.owner)
		}
	}
	got := CanonicalRepos([]string{"git@github.com:acme/api.git", "https://github.com/acme/web", "acme/api"})
	want := "github.com/acme/api,github.com/acme/web"
	if strings.Join(got, ",") != want {
		t.Fatalf("CanonicalRepos = %v, want %s", got, want)
	}
}

func TestDecodeErrorRemediation(t *testing.T) {
	body := []byte(`{"error":{"code":"secret_name_ambiguous","message":"NPM_TOKEN has 2 versions","versions":[{"id":"secv_aaaaaaaaaaaaaaaaaaaaaa","repos":[]},{"id":"secv_bbbbbbbbbbbbbbbbbbbbbb","repos":["github.com/acme/api"]}]}}`)
	err := decodeError(409, body, true)
	text := err.Error()
	for _, needle := range []string{"secret_name_ambiguous", "secv_aaaaaaaaaaaaaaaaaaaaaa", "secv_bbbbbbbbbbbbbbbbbbbbbb", "several versions"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("error %q missing %q", text, needle)
		}
	}
	codes := []string{"team_admin_required", "secret_name_conflict", "secret_scope_conflict", "feature_unavailable", "repository_access", "environment_name_conflict", "validation_error"}
	for _, code := range codes {
		err := decodeError(403, []byte(`{"error":{"code":"`+code+`","message":"no"}}`), false)
		if !strings.Contains(err.Error(), code) || remediation(code) == "" || !strings.Contains(err.Error(), remediation(code)[:12]) {
			t.Fatalf("code %s error = %s", code, err)
		}
	}
}

func TestListEnvironmentsPagination(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.Header.Get("Authorization") == "" || r.Header.Get("User-Agent") != "terraform-provider-cursor/test+braze" {
			t.Errorf("headers = %v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = io.WriteString(w, `{"items":[{"id":"1","owner":"team","name":"a","repos":[],"createdAt":"t","updatedAt":"t"}]}`)
			// nextCursor omitted means stop when we don't write it. Write it for page 1.
			w.Header().Set("Content-Type", "application/json")
		default:
			_, _ = io.WriteString(w, `{"items":[{"id":"2","owner":"team","name":"b","repos":[],"createdAt":"t","updatedAt":"t"}]}`)
		}
	}))
	defer server.Close()
	// First response must include nextCursor. Replace handler.
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RawQuery)
		if r.URL.Query().Get("cursor") == "" {
			_, _ = io.WriteString(w, `{"items":[{"id":"1","owner":"team","repos":[],"createdAt":"t","updatedAt":"t"}],"nextCursor":"page-2"}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"2","owner":"team","repos":[],"createdAt":"t","updatedAt":"t"}]}`)
	})
	client := New(Options{BaseURL: server.URL, APIKey: "key_test", UserAgent: "terraform-provider-cursor/test+braze"})
	envs, err := client.ListEnvironments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 2 || envs[1].ID != "2" {
		t.Fatalf("envs = %#v", envs)
	}
	if len(paths) != 2 || !strings.Contains(paths[1], "cursor=page-2") {
		t.Fatalf("queries = %v", paths)
	}
}

func TestUpdateEnvironmentOmitsRepos(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		if strings.Contains(body, "repos") || strings.Contains(body, "owner") {
			t.Errorf("PATCH body included a rejected field: %s", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := New(Options{BaseURL: server.URL, APIKey: "key_test", UserAgent: "terraform-provider-cursor/dev+braze"})
	name := "renamed"
	if err := client.UpdateEnvironment(context.Background(), "env-1", UpdateEnvironmentRequest{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"name":"renamed"`) {
		t.Fatalf("body = %s", body)
	}
}
