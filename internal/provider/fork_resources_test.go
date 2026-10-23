package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	pschema "github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestResolveForkKeysCapturesRawKeyBeforeUse(t *testing.T) {
	api, pool := resolveForkKeys("key_raw", "", "")
	if api != "key_raw" || pool != "key_raw" {
		t.Fatalf("api=%q pool=%q", api, pool)
	}
	api, pool = resolveForkKeys("session", "key_explicit", "")
	if api != "key_explicit" || pool != "key_explicit" {
		t.Fatalf("explicit api=%q pool=%q", api, pool)
	}
	api, pool = resolveForkKeys("key_raw", "", "key_pool")
	if api != "key_raw" || pool != "key_pool" {
		t.Fatalf("pool override api=%q pool=%q", api, pool)
	}
	if _, pool := resolveForkKeys("session-token", "", ""); pool != "" {
		t.Fatalf("session token should not become a pool key, got %q", pool)
	}
}

func TestConfigureTreatsOrganizationAPIKeyAsCredential(t *testing.T) {
	ctx := context.Background()
	p := &cursorProvider{version: "test"}
	schemaResp := &provider.SchemaResponse{}
	p.Schema(ctx, provider.SchemaRequest{}, schemaResp)

	cases := []struct {
		name    string
		model   cursorProviderModel
		envKey  string
		envVal  string
		wantErr bool
		orgKey  string
	}{
		{
			name:   "attribute",
			model:  cursorProviderModel{OrganizationAPIKey: types.StringValue("org-key"), OrganizationID: types.StringValue("org_1")},
			orgKey: "org-key",
		},
		{
			name:   "environment",
			envKey: envOrgAPIKey,
			envVal: "env-org-key",
			orgKey: "env-org-key",
		},
		{name: "absent", wantErr: true},
		{
			name:    "organization id only",
			model:   cursorProviderModel{OrganizationID: types.StringValue("org_1")},
			wantErr: true,
		},
		{
			name:    "blank attribute",
			model:   cursorProviderModel{OrganizationAPIKey: types.StringValue("   ")},
			wantErr: true,
		},
		{
			name:    "blank environment",
			envKey:  envOrgAPIKey,
			envVal:  "  ",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{envToken, envAPIKey, envTeamAPIKey, envOrgAPIKey, envPoolAPIKey, envOrgID} {
				t.Setenv(key, "")
			}
			if tc.envKey != "" {
				t.Setenv(tc.envKey, tc.envVal)
			}
			resp := configureCursor(t, ctx, p, schemaResp.Schema, tc.model)
			if tc.wantErr {
				if !resp.Diagnostics.HasError() {
					t.Fatal("expected missing credential diagnostic")
				}
				detail := resp.Diagnostics.Errors()[0].Detail()
				if !strings.Contains(detail, "organization_api_key") {
					t.Fatalf("detail = %q", detail)
				}
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatal(resp.Diagnostics)
			}
			client, ok := resp.ResourceData.(*apiClient)
			if !ok {
				t.Fatalf("resource data = %T", resp.ResourceData)
			}
			if client.orgAPIKey != tc.orgKey || client.authHeader != "" {
				t.Fatalf("orgAPIKey = %q auth = %q", client.orgAPIKey, client.authHeader)
			}
		})
	}
}

func configureCursor(t *testing.T, ctx context.Context, p *cursorProvider, sch pschema.Schema, model cursorProviderModel) *provider.ConfigureResponse {
	t.Helper()
	state := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if diags := state.Set(ctx, model); diags.HasError() {
		t.Fatal(diags)
	}
	resp := &provider.ConfigureResponse{}
	p.Configure(ctx, provider.ConfigureRequest{Config: tfsdk.Config{Schema: sch, Raw: state.Raw}}, resp)
	return resp
}

func TestNormalizeEnvironmentJSON(t *testing.T) {
	got, err := normalizeEnvironmentJSON("{\n  \"start\": \"up\", \"install\": \"pnpm\" }")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"install":"pnpm","start":"up"}` {
		t.Fatalf("normalized = %s", got)
	}
}

func TestEnvironmentCreateUpdateDeleteAndRepoFile(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	env := map[string]any{
		"id": "11111111-1111-1111-1111-111111111111", "owner": "team", "name": "web",
		"repos":           []any{map[string]any{"url": "https://github.com/acme/api"}},
		"environmentJson": `{"install":"pnpm"}`, "createdAt": "t", "updatedAt": "t",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.Method+" "+r.URL.Path+" "+string(raw))
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments":
			if strings.Contains(string(raw), `"name":"conflict"`) {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"code":"environment_name_conflict","message":"name taken"}}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(env)
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(env)
		case r.Method == http.MethodPatch:
			if strings.Contains(string(raw), "repos") || strings.Contains(string(raw), "owner") {
				t.Errorf("PATCH rejected fields: %s", raw)
			}
			var patch map[string]any
			_ = json.Unmarshal(raw, &patch)
			if name, ok := patch["name"].(string); ok {
				env["name"] = name
			}
			if rawJSON, ok := patch["environmentJson"].(string); ok {
				env["environmentJson"] = rawJSON
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"11111111-1111-1111-1111-111111111111"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	res := &environmentResource{client: publicClient(server.URL, "key_test")}
	ctx := context.Background()
	sch := resourceSchema(t, res)
	repos, diags := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: map[string]attr.Type{"url": types.StringType}}, []environmentRepoModel{{URL: types.StringValue("https://github.com/acme/api")}})
	if diags.HasError() {
		t.Fatal(diags)
	}
	plan := environmentModel{
		ID:              types.StringUnknown(),
		Owner:           types.StringValue("team"),
		Name:            types.StringValue("web"),
		EnvironmentJSON: types.StringValue(`{"install":"pnpm"}`),
		Repos:           mustRepos(t, repos),
	}
	// Repos is a slice, set it directly.
	plan.Repos = []environmentRepoModel{{URL: types.StringValue("https://github.com/acme/api")}}
	state := mustApply(t, ctx, sch, func(resp *resource.CreateResponse) {
		res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	})
	var got environmentModel
	if diags := state.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.ID.ValueString() != env["id"] || got.EnvironmentJSON.ValueString() != `{"install":"pnpm"}` {
		t.Fatalf("state = %#v", got)
	}

	plan.Name = types.StringValue("web-2")
	plan.EnvironmentJSON = types.StringValue(`{"install":"pnpm","start":"up"}`)
	plan.ID = got.ID
	update := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: state}, update)
	if update.Diagnostics.HasError() {
		t.Fatal(update.Diagnostics)
	}
	mu.Lock()
	patched := strings.Join(bodies, "\n")
	mu.Unlock()
	if !strings.Contains(patched, `"name":"web-2"`) || !strings.Contains(patched, `"environmentJson"`) {
		t.Fatalf("expected one PATCH with both fields, bodies:\n%s", patched)
	}

	env["repoFile"] = map[string]any{"url": "https://github.com/acme/api", "path": ".cursor/environment.json"}
	plan.Name = types.StringValue("web-3")
	blocked := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: update.State}, blocked)
	if !blocked.Diagnostics.HasError() || !strings.Contains(blocked.Diagnostics.Errors()[0].Detail(), "managed in git") {
		t.Fatalf("repoFile update diagnostics = %v", blocked.Diagnostics)
	}
	delete(env, "repoFile")

	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"environment_not_found","message":"missing"}}`)
	}))
	defer missing.Close()
	res.client = publicClient(missing.URL, "key_test")
	read := &resource.ReadResponse{State: update.State}
	res.Read(ctx, resource.ReadRequest{State: update.State}, read)
	if !read.State.Raw.IsNull() {
		t.Fatal("404 should remove the environment from state")
	}

	conflict := &environmentResource{client: publicClient(server.URL, "key_test")}
	bad := plan
	bad.Name = types.StringValue("conflict")
	badResp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	conflict.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &bad)}, badResp)
	if !badResp.Diagnostics.HasError() || !strings.Contains(fmtDiag(badResp.Diagnostics), "environment_name_conflict") {
		t.Fatalf("conflict diagnostics = %v", badResp.Diagnostics)
	}
}

func TestEnvironmentFeatureAndValidationErrors(t *testing.T) {
	for _, code := range []string{"feature_unavailable", "validation_error"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if code == "validation_error" {
				w.WriteHeader(http.StatusBadRequest)
			} else {
				w.WriteHeader(http.StatusForbidden)
			}
			_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":"no"}}`)
		}))
		res := &environmentResource{client: publicClient(server.URL, "key_test")}
		ctx := context.Background()
		sch := resourceSchema(t, res)
		plan := environmentModel{Owner: types.StringValue("team"), Name: types.StringValue("web"), EnvironmentJSON: types.StringValue(`{}`), Repos: []environmentRepoModel{}}
		resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
		res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
		if !strings.Contains(fmtDiag(resp.Diagnostics), code) {
			t.Fatalf("%s diagnostics = %v", code, resp.Diagnostics)
		}
		server.Close()
	}
}

func TestSecretLifecycleCanonicalReposAndLeak(t *testing.T) {
	secretValueWant := "super-secret-value"
	version := map[string]any{"id": "secv_3qKx9mT2bV7nR1cW5yZ8aQ", "name": "NPM_TOKEN", "type": "runtime_secret", "repos": []string{"github.com/acme/api"}, "createdAt": "t"}
	var putBody string
	var status = http.StatusCreated
	omitID := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPut:
			putBody = string(raw)
			if strings.Contains(r.URL.Path, "conflict-case") {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"code":"secret_name_conflict","message":"case"}}`)
				return
			}
			putBody = r.Method + " " + r.URL.String() + " " + putBody
			if strings.Contains(putBody, "overlap") {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":{"code":"secret_scope_conflict","message":"overlap"}}`)
				return
			}
			w.WriteHeader(status)
			out := map[string]any{}
			for k, v := range version {
				out[k] = v
			}
			if omitID {
				delete(out, "id")
				delete(out, "createdAt")
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet:
			item := map[string]any{}
			for k, v := range version {
				item[k] = v
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{item}, "nextCursor": nil})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"name":"NPM_TOKEN"}`)
		}
	}))
	defer server.Close()
	res := &secretResource{client: publicClient(server.URL, "key_test"), scope: secretScopeEnvironment}
	ctx := context.Background()
	sch := resourceSchema(t, res)
	repos, _ := types.ListValueFrom(ctx, types.StringType, []string{"acme/api", "https://github.com/acme/api.git", "git@github.com:acme/api.git"})
	plan := secretModel{
		EnvironmentID:  types.StringValue("11111111-1111-1111-1111-111111111111"),
		Name:           types.StringValue("NPM_TOKEN"),
		Type:           types.StringValue("runtime_secret"),
		Repos:          repos,
		ValueWOVersion: types.Int64Value(1),
		ValueWO:        types.StringNull(),
	}
	cfg := plan
	cfg.ValueWO = types.StringValue(secretValueWant)
	omitID = true
	create := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan), Config: mustConfig(t, ctx, sch, &cfg)}, create)
	if create.Diagnostics.HasError() {
		t.Fatal(create.Diagnostics)
	}
	raw, err := json.Marshal(create.State.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secretValueWant) || strings.Contains(putBody, "acme/api") && !strings.Contains(putBody, "github.com/acme/api") {
		t.Fatalf("state leaked or repos were not canonical: body %s state %s", putBody, raw)
	}
	if !strings.Contains(putBody, secretValueWant) || strings.Count(putBody, "github.com/acme/api") < 1 {
		t.Fatalf("put body = %s", putBody)
	}
	var got secretModel
	if diags := create.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.SecretVersionID.ValueString() != version["id"] || got.ValueWO.ValueString() != "" && !got.ValueWO.IsNull() {
		t.Fatalf("version = %#v", got)
	}

	omitID = false
	version["type"] = "environment_variable"
	plan.Type = types.StringValue("environment_variable")
	plan.SecretVersionID = got.SecretVersionID
	plan.ID = got.ID
	update := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: create.State, Config: mustConfig(t, ctx, sch, &cfg)}, update)
	if update.Diagnostics.HasError() {
		t.Fatal(update.Diagnostics)
	}
	if !strings.Contains(putBody, `"type":"environment_variable"`) || !strings.Contains(putBody, secretValueWant) {
		t.Fatalf("update body = %s", putBody)
	}

	del := &resource.DeleteResponse{}
	res.Delete(ctx, resource.DeleteRequest{State: update.State}, del)
	if del.Diagnostics.HasError() {
		t.Fatal(del.Diagnostics)
	}

	version["name"] = "conflict-case"
	plan.Name = types.StringValue("conflict-case")
	bad := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan), Config: mustConfig(t, ctx, sch, &cfg)}, bad)
	if !strings.Contains(fmtDiag(bad.Diagnostics), "secret_name_conflict") {
		t.Fatalf("conflict = %v", bad.Diagnostics)
	}
}

func TestSecretErrorsAndBuildSecret(t *testing.T) {
	ctx := context.Background()
	ambiguous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"secret_name_ambiguous","message":"pick one","versions":[{"id":"secv_aaaaaaaaaaaaaaaaaaaaaa","repos":[]},{"id":"secv_bbbbbbbbbbbbbbbbbbbbbb","repos":[]}]}}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"team_admin_required","message":"admins only"}}`)
	}))
	defer ambiguous.Close()
	res := &secretResource{client: publicClient(ambiguous.URL, "key_test"), scope: secretScopeTeam}
	sch := resourceSchema(t, res)
	repos, _ := types.ListValueFrom(ctx, types.StringType, []string{})
	plan := teamSecretModel{Name: types.StringValue("NPM_TOKEN"), Type: types.StringValue("runtime_secret"), Repos: repos, ValueWOVersion: types.Int64Value(1)}
	cfg := plan
	cfg.ValueWO = types.StringValue("x")
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan), Config: mustConfig(t, ctx, sch, &cfg)}, resp)
	if !strings.Contains(fmtDiag(resp.Diagnostics), "secret_name_ambiguous") || !strings.Contains(fmtDiag(resp.Diagnostics), "secv_aaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatalf("ambiguous = %v", resp.Diagnostics)
	}

	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"team_admin_required","message":"admins only"}}`)
	}))
	defer admin.Close()
	res.client = publicClient(admin.URL, "key_test")
	resp = &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan), Config: mustConfig(t, ctx, sch, &cfg)}, resp)
	if !strings.Contains(fmtDiag(resp.Diagnostics), "team_admin_required") || !strings.Contains(fmtDiag(resp.Diagnostics), "only admins") {
		t.Fatalf("admin = %v", resp.Diagnostics)
	}

	access := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"repository_access","message":"no repo"}}`)
	}))
	defer access.Close()
	res.client = publicClient(access.URL, "key_test")
	resp = &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan), Config: mustConfig(t, ctx, sch, &cfg)}, resp)
	if !strings.Contains(fmtDiag(resp.Diagnostics), "repository_access") {
		t.Fatalf("access = %v", resp.Diagnostics)
	}

	overlap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"secret_scope_conflict","message":"overlap"}}`)
	}))
	defer overlap.Close()
	res.client = publicClient(overlap.URL, "key_test")
	resp = &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan), Config: mustConfig(t, ctx, sch, &cfg)}, resp)
	if !strings.Contains(fmtDiag(resp.Diagnostics), "secret_scope_conflict") || !strings.Contains(fmtDiag(resp.Diagnostics), "overlap another version") {
		t.Fatalf("scope = %v", resp.Diagnostics)
	}

	build := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"id":"secv_3qKx9mT2bV7nR1cW5yZ8aQ","name":"NPM_TOKEN","type":"build_secret","repos":[],"createdAt":"t"}],"nextCursor":null}`)
	}))
	defer build.Close()
	res.client = publicClient(build.URL, "key_test")
	prior := plan
	prior.ID = types.StringValue("secv_3qKx9mT2bV7nR1cW5yZ8aQ")
	prior.SecretVersionID = prior.ID
	state := tfsdk.State{Schema: sch}
	if diags := state.Set(ctx, &prior); diags.HasError() {
		t.Fatal(diags)
	}
	read := &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, read)
	if !strings.Contains(fmtDiag(read.Diagnostics), "Build Secret") {
		t.Fatalf("build secret = %v", read.Diagnostics)
	}
}

func TestSecretImportAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"id":"secv_aaaaaaaaaaaaaaaaaaaaaa","name":"NPM_TOKEN","type":"runtime_secret","repos":[],"createdAt":"t"},{"id":"secv_bbbbbbbbbbbbbbbbbbbbbb","name":"NPM_TOKEN","type":"runtime_secret","repos":["github.com/acme/api"],"createdAt":"t"}],"nextCursor":null}`)
	}))
	defer server.Close()
	res := &secretResource{client: publicClient(server.URL, "key_test"), scope: secretScopeEnvironment}
	ctx := context.Background()
	sch := resourceSchema(t, res)
	imp := &resource.ImportStateResponse{State: emptyState(ctx, sch)}
	res.ImportState(ctx, resource.ImportStateRequest{ID: "11111111-1111-1111-1111-111111111111/NPM_TOKEN"}, imp)
	if imp.Diagnostics.HasError() {
		t.Fatal(imp.Diagnostics)
	}
	read := &resource.ReadResponse{State: imp.State}
	res.Read(ctx, resource.ReadRequest{State: imp.State}, read)
	if !strings.Contains(fmtDiag(read.Diagnostics), "secv_aaaaaaaaaaaaaaaaaaaaaa") || !strings.Contains(fmtDiag(read.Diagnostics), "secv_bbbbbbbbbbbbbbbbbbbbbb") {
		t.Fatalf("import ambiguous = %v", read.Diagnostics)
	}
}

func TestEnvironmentAndPoolImport(t *testing.T) {
	ctx := context.Background()
	envServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"11111111-1111-1111-1111-111111111111","owner":"team","name":"web","repos":[{"url":"https://github.com/acme/api"}],"environmentJson":"{}","createdAt":"t","updatedAt":"t"}`)
	}))
	defer envServer.Close()
	envRes := &environmentResource{client: publicClient(envServer.URL, "key_test")}
	envSchema := resourceSchema(t, envRes)
	imported := &resource.ImportStateResponse{State: emptyState(ctx, envSchema)}
	envRes.ImportState(ctx, resource.ImportStateRequest{ID: "11111111-1111-1111-1111-111111111111"}, imported)
	if imported.Diagnostics.HasError() {
		t.Fatal(imported.Diagnostics)
	}
	read := &resource.ReadResponse{State: imported.State}
	envRes.Read(ctx, resource.ReadRequest{State: imported.State}, read)
	if read.Diagnostics.HasError() {
		t.Fatal(read.Diagnostics)
	}
	var env environmentModel
	if diags := read.State.Get(ctx, &env); diags.HasError() || env.Name.ValueString() != "web" || len(env.Repos) != 1 {
		t.Fatalf("imported environment %#v %v", env, diags)
	}

	poolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "team_pool" {
			t.Errorf("list scope = %s", r.URL.RawQuery)
		}
		_, _ = io.WriteString(w, `{"pools":[{"scope":"team","poolName":"gpu","repoOwner":"acme","repoName":"api","workerReadyTimeoutSeconds":0,"connectedWorkerCount":1,"inUseWorkerCount":0,"isStale":false}]}`)
	}))
	defer poolServer.Close()
	poolRes := &privateWorkerPoolResource{client: poolClient(poolServer.URL, "key_pool")}
	poolSchema := resourceSchema(t, poolRes)
	poolImport := &resource.ImportStateResponse{State: emptyState(ctx, poolSchema)}
	poolRes.ImportState(ctx, resource.ImportStateRequest{ID: "team/gpu/acme/api"}, poolImport)
	if poolImport.Diagnostics.HasError() {
		t.Fatal(poolImport.Diagnostics)
	}
	poolRead := &resource.ReadResponse{State: poolImport.State}
	poolRes.Read(ctx, resource.ReadRequest{State: poolImport.State}, poolRead)
	if poolRead.Diagnostics.HasError() {
		t.Fatal(poolRead.Diagnostics)
	}
	var pool privateWorkerPoolModel
	if diags := poolRead.State.Get(ctx, &pool); diags.HasError() || pool.ID.ValueString() != "team/gpu/acme/api" || pool.ConnectedWorkerCount.ValueInt64() != 1 {
		t.Fatalf("imported pool %#v %v", pool, diags)
	}
}

func TestPoolRegisterHasNoIDAndDeregisters(t *testing.T) {
	var gotBody, gotDelete string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch r.Method {
		case http.MethodPost:
			gotBody = string(raw)
			_, _ = io.WriteString(w, `{"registered":true}`)
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"pools":[{"scope":"team","poolName":"gpu","workerReadyTimeoutSeconds":0,"connectedWorkerCount":1,"inUseWorkerCount":0,"isStale":false}]}`)
		case http.MethodDelete:
			gotDelete = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"deregistered":true}`)
		}
	}))
	defer server.Close()
	res := &privateWorkerPoolResource{client: poolClient(server.URL, "key_pool")}
	ctx := context.Background()
	sch := resourceSchema(t, res)
	plan := privateWorkerPoolModel{
		Scope: types.StringValue("team"), PoolName: types.StringValue("gpu"),
		WorkerReadyTimeoutSeconds: types.Int64Value(0),
		RepoOwner:                 types.StringNull(), RepoName: types.StringNull(), RepoURL: types.StringNull(),
	}
	create := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, create)
	if create.Diagnostics.HasError() {
		t.Fatal(create.Diagnostics)
	}
	var got privateWorkerPoolModel
	if diags := create.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.ID.ValueString() != "team/gpu" {
		t.Fatalf("id = %s body = %s", got.ID.ValueString(), gotBody)
	}
	if strings.Contains(gotBody, "repoOwner") || !strings.Contains(gotBody, `"registered"`) && !strings.Contains(gotBody, `"poolName":"gpu"`) {
		t.Fatalf("register body = %s", gotBody)
	}
	del := &resource.DeleteResponse{}
	res.Delete(ctx, resource.DeleteRequest{State: create.State}, del)
	if del.Diagnostics.HasError() || !strings.Contains(gotDelete, "pool_name=gpu") || !strings.Contains(gotDelete, "scope=team") {
		t.Fatalf("delete query = %s diags = %v", gotDelete, del.Diagnostics)
	}
}

func TestCreateEnvironmentRetries503(t *testing.T) {
	var n int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"upstream_error","message":"source control"}}`)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"env","owner":"team","name":"web","repos":[],"createdAt":"t","updatedAt":"t"}`)
	}))
	defer server.Close()
	server.Config.SetKeepAlivesEnabled(false)
	client := publicapi.New(publicapi.Options{
		BaseURL:   server.URL,
		APIKey:    "key_test",
		UserAgent: "terraform-provider-cursor/test+braze",
		HTTP:      &retryClient{next: server.Client(), wait: func(context.Context, time.Duration) error { return nil }},
	})
	env, err := client.CreateEnvironment(context.Background(), publicapi.CreateEnvironmentRequest{Owner: "team", Name: "web", Repos: []publicapi.EnvironmentRepo{}, EnvironmentJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if env.ID != "env" || n != 2 {
		t.Fatalf("env=%#v requests=%d", env, n)
	}
}

func TestGuardrailsRejectAutomation(t *testing.T) {
	ctx := context.Background()
	res := &platformWorkflowResource{client: &apiClient{guardrails: &guardrailsConfig{
		HasAllowedScopes: true,
		AllowedScopes:    []string{"team"},
		RequirePoolLabel: true,
		HasAllowedModels: true,
		AllowedModels:    []string{"composer-2"},
		HasAllowedOwners: true,
		AllowedOwners:    []string{"acme"},
		DenyPRApproval:   true,
	}}}
	sch := resourceSchema(t, res)
	repos, _ := types.ListValueFrom(ctx, types.StringType, []string{"other/api"})
	labels, _ := types.MapValueFrom(ctx, types.StringType, map[string]string{"repo": "acme/api"})
	model := platformWorkflowModel{
		Name:                 types.StringValue("review"),
		Prompt:               types.StringValue("review"),
		Scope:                types.StringNull(),
		Model:                types.StringNull(),
		GitRepo:              types.StringNull(),
		DisabledDefaultTools: types.ListNull(types.StringType),
		PrivateWorker:        &privateWorkerModel{Labels: labels},
		Triggers: []triggerModel{{
			GitPullRequest: &gitPullRequestModel{Repos: repos, Orgs: types.ListNull(types.StringType)},
			UserAllowlist:  types.ListNull(types.StringType),
		}},
		Actions: []actionModel{{
			PrComment: &prCommentActionModel{AllowInlineComments: types.BoolNull(), AllowApprove: types.BoolValue(true)},
			ApprovePr: &approvePrActionModel{},
		}},
	}
	resp := &resource.ModifyPlanResponse{Plan: mustPlan(t, ctx, sch, &model)}
	res.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: resp.Plan, Config: mustConfig(t, ctx, sch, &model)}, resp)
	text := fmtDiag(resp.Diagnostics)
	for _, needle := range []string{"scope", "pool", "repo", "model", "allow_approve", "approve_pr", "owner"} {
		if !strings.Contains(strings.ToLower(text), needle) {
			t.Fatalf("diagnostics missing %s: %s", needle, text)
		}
	}
}

func TestGuardrailsWaitForUnknownModel(t *testing.T) {
	ctx := context.Background()
	res := &platformWorkflowResource{client: &apiClient{guardrails: &guardrailsConfig{HasAllowedModels: true, AllowedModels: []string{"composer-2"}}}}
	sch := resourceSchema(t, res)
	model := platformWorkflowModel{
		Name: types.StringValue("review"), Prompt: types.StringValue("review"),
		Model: types.StringUnknown(), Scope: types.StringValue("team"),
		DisabledDefaultTools: types.ListNull(types.StringType),
		Triggers:             []triggerModel{{GitPullRequest: &gitPullRequestModel{Repos: types.ListNull(types.StringType), Orgs: types.ListNull(types.StringType)}, UserAllowlist: types.ListNull(types.StringType)}},
	}
	resp := &resource.ModifyPlanResponse{Plan: mustPlan(t, ctx, sch, &model)}
	res.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: resp.Plan, Config: mustConfig(t, ctx, sch, &model)}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unknown model should wait: %v", resp.Diagnostics)
	}
}

func TestModelAccessConflictAndSpendClear(t *testing.T) {
	var spendBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(r.URL.Path, "/model-access/providers"):
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"code":"error","message":"unrestricted"}`)
		case r.URL.Path == "/teams/user-spend-limit":
			spendBody = string(raw)
			_, _ = io.WriteString(w, `{"outcome":"success","message":"cleared"}`)
		case r.URL.Path == "/teams/spend":
			_, _ = io.WriteString(w, `{"teamMemberSpend":[{"email":"a@acme.com","monthlyLimitDollars":null,"hardLimitOverrideDollars":0}],"totalPages":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &apiClient{httpClient: server.Client(), adminBase: server.URL, teamAPIKey: "team-key", userAgent: "test"}
	_, err := client.listModelAccessProviders(context.Background())
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "cursor_model_access_configuration") {
		t.Fatalf("model access err = %v", err)
	}
	res := &userSpendLimitResource{client: client}
	ctx := context.Background()
	sch := resourceSchema(t, res)
	plan := userSpendLimitModel{UserEmail: types.StringValue("a@acme.com"), SpendLimitDollars: types.Int64Null()}
	create := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, create)
	if create.Diagnostics.HasError() {
		t.Fatal(create.Diagnostics)
	}
	if !strings.Contains(spendBody, `"spendLimitDollars":null`) {
		t.Fatalf("spend body = %s", spendBody)
	}
	var got userSpendLimitModel
	if diags := create.State.Get(ctx, &got); diags.HasError() || !got.SpendLimitDollars.IsNull() {
		t.Fatalf("state limit %#v diags %v", got.SpendLimitDollars, diags)
	}
	zero := int64(0)
	plan.SpendLimitDollars = types.Int64Value(zero)
	update := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	// Refresh still returns null, so the test only checks the request body.
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/teams/user-spend-limit" {
			spendBody = string(raw)
			_, _ = io.WriteString(w, `{"outcome":"success"}`)
			return
		}
		_, _ = io.WriteString(w, `{"teamMemberSpend":[{"email":"a@acme.com","monthlyLimitDollars":0,"hardLimitOverrideDollars":0}],"totalPages":1}`)
	})
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: create.State}, update)
	if update.Diagnostics.HasError() || !strings.Contains(spendBody, `"spendLimitDollars":0`) {
		t.Fatalf("zero body %s diags %v", spendBody, update.Diagnostics)
	}
}

func TestDirectoryGroupClearsLimitAndBlocklist(t *testing.T) {
	var patchBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/teams/directory-groups":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"group":{"id":"team_group_1","name":"Eng","memberCount":0,"monthlySpendingLimitDollars":null}}`)
		case r.Method == http.MethodPatch:
			patchBody = string(raw)
			_, _ = io.WriteString(w, `{"group":{"id":"team_group_1","name":"Eng","memberCount":0,"monthlySpendingLimitDollars":null}}`)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "upsert"):
			_, _ = io.WriteString(w, `{"repos":[{"id":"repo_123","url":"https://github.com/acme/api","patterns":["*.env"]}]}`)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "repo_123"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &apiClient{httpClient: server.Client(), adminBase: server.URL, teamAPIKey: "team-key"}
	group := &groupResource{client: client, kind: groupKindTeam}
	ctx := context.Background()
	sch := resourceSchema(t, group)
	prior := groupModel{ID: types.StringValue("team_group_1"), Name: types.StringValue("Eng"), MonthlySpendingLimitDollars: types.Int64Value(10), MemberCount: types.Int64Value(0)}
	plan := prior
	plan.MonthlySpendingLimitDollars = types.Int64Null()
	update := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	group.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: mustState(t, ctx, sch, &prior)}, update)
	if update.Diagnostics.HasError() || !strings.Contains(patchBody, `"clearMonthlySpendingLimitDollars":true`) || strings.Contains(patchBody, "monthlySpendingLimitDollars") {
		t.Fatalf("patch %s diags %v", patchBody, update.Diagnostics)
	}

	block := &repoBlocklistResource{client: client}
	bsch := resourceSchema(t, block)
	patterns, _ := types.ListValueFrom(ctx, types.StringType, []string{"*.env"})
	bplan := repoBlocklistModel{URL: types.StringValue("https://github.com/acme/api"), Patterns: patterns}
	create := &resource.CreateResponse{State: tfsdk.State{Schema: bsch}}
	block.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, schWait(bsch), &bplan)}, create)
	if create.Diagnostics.HasError() {
		t.Fatal(create.Diagnostics)
	}
	var got repoBlocklistModel
	if diags := create.State.Get(ctx, &got); diags.HasError() || got.ID.ValueString() != "repo_123" {
		t.Fatalf("blocklist %#v %v", got, diags)
	}
}

func TestAPIKeyInfoServiceAccountAndMembers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me" {
			_, _ = io.WriteString(w, `{"apiKeyName":"terraform","createdAt":"2026-01-01T00:00:00Z"}`)
			return
		}
		_, _ = io.WriteString(w, `{"teamMembers":[{"id":"user_1","email":"a@acme.com","name":"A","role":"owner","isRemoved":false}]}`)
	}))
	defer server.Close()
	info := &apiKeyInfoDataSource{client: publicClient(server.URL, "key_test")}
	ctx := context.Background()
	sch := dataSchema(t, info)
	read := newDataRead(sch)
	info.Read(ctx, read.req, read.resp)
	if read.resp.Diagnostics.HasError() {
		t.Fatal(read.resp.Diagnostics)
	}
	var got apiKeyInfoModel
	if diags := read.resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.APIKeyName.ValueString() != "terraform" || !got.UserEmail.IsNull() || !got.UserID.IsNull() {
		t.Fatalf("service account info = %#v", got)
	}
	members := &teamMembersDataSource{client: &apiClient{httpClient: server.Client(), adminBase: server.URL, teamAPIKey: "team"}}
	msch := dataSchema(t, members)
	mread := newDataRead(msch)
	members.Read(ctx, mread.req, mread.resp)
	if mread.resp.Diagnostics.HasError() {
		t.Fatal(mread.resp.Diagnostics)
	}
	var listed teamMembersModel
	if diags := mread.resp.State.Get(ctx, &listed); diags.HasError() || len(listed.Members) != 1 || listed.Members[0].Role.ValueString() != "owner" {
		t.Fatalf("members %#v %v", listed, diags)
	}
}

func TestCloudAgentDataSources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/builds/active"):
			_, _ = io.WriteString(w, `{"type":"build","buildId":"bld_1"}`)
		case strings.Contains(r.URL.Path, "/builds/"):
			_, _ = io.WriteString(w, `{"id":"bld_1","status":"SUCCEEDED"}`)
		case r.URL.Path == "/v1/environments":
			_, _ = io.WriteString(w, `{"items":[{"id":"env-1","owner":"team","name":"web","repos":[{"url":"https://github.com/acme/api"}],"createdAt":"t","updatedAt":"t"}]}`)
		case r.URL.Path == "/v1/models":
			_, _ = io.WriteString(w, `{"items":[{"id":"composer-2","displayName":"Composer","parameters":[],"variants":[]}]}`)
		case r.URL.Path == "/v0/private-workers/pools":
			_, _ = io.WriteString(w, `{"pools":[{"scope":"team","poolName":"gpu","workerReadyTimeoutSeconds":0,"connectedWorkerCount":2,"inUseWorkerCount":1,"isStale":false}]}`)
		case r.URL.Path == "/v0/private-workers/summary":
			_, _ = io.WriteString(w, `{"teamSummary":{"totalConnected":2,"inUse":1},"userSummary":{"totalConnected":0,"inUse":0}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	client := publicClient(server.URL, "key_test")

	envs := &environmentsDataSource{client: client}
	envRead := newDataRead(dataSchema(t, envs))
	envs.Read(ctx, envRead.req, envRead.resp)
	if envRead.resp.Diagnostics.HasError() {
		t.Fatal(envRead.resp.Diagnostics)
	}
	var listed environmentsModel
	if diags := envRead.resp.State.Get(ctx, &listed); diags.HasError() || len(listed.Environments) != 1 || listed.Environments[0].Name.ValueString() != "web" {
		t.Fatalf("environments %#v %v", listed, diags)
	}

	build := &environmentActiveBuildDataSource{client: client}
	buildSchema := dataSchema(t, build)
	buildRead := newDataRead(buildSchema)
	buildRead.req.Config = tfsdk.Config{Schema: buildSchema, Raw: mustDataState(t, ctx, buildSchema, &environmentActiveBuildModel{EnvironmentID: types.StringValue("env-1")}).Raw}
	build.Read(ctx, buildRead.req, buildRead.resp)
	if buildRead.resp.Diagnostics.HasError() {
		t.Fatal(buildRead.resp.Diagnostics)
	}
	var active environmentActiveBuildModel
	if diags := buildRead.resp.State.Get(ctx, &active); diags.HasError() || active.Status.ValueString() != "SUCCEEDED" || active.BuildID.ValueString() != "bld_1" {
		t.Fatalf("active build %#v %v", active, diags)
	}

	models := &modelsDataSource{client: client}
	modelRead := newDataRead(dataSchema(t, models))
	models.Read(ctx, modelRead.req, modelRead.resp)
	if modelRead.resp.Diagnostics.HasError() {
		t.Fatal(modelRead.resp.Diagnostics)
	}
	var catalog modelsModel
	if diags := modelRead.resp.State.Get(ctx, &catalog); diags.HasError() || len(catalog.Models) != 1 || catalog.Models[0].ID.ValueString() != "composer-2" {
		t.Fatalf("models %#v %v", catalog, diags)
	}

	pools := &privateWorkerPoolsDataSource{client: client}
	poolSchema := dataSchema(t, pools)
	poolRead := newDataRead(poolSchema)
	poolRead.req.Config = tfsdk.Config{Schema: poolSchema, Raw: mustDataState(t, ctx, poolSchema, &privateWorkerPoolsModel{Scope: types.StringValue("team_pool")}).Raw}
	pools.Read(ctx, poolRead.req, poolRead.resp)
	if poolRead.resp.Diagnostics.HasError() {
		t.Fatal(poolRead.resp.Diagnostics)
	}
	var poolList privateWorkerPoolsModel
	if diags := poolRead.resp.State.Get(ctx, &poolList); diags.HasError() || len(poolList.Pools) != 1 || poolList.Pools[0].PoolName.ValueString() != "gpu" || poolList.Scope.ValueString() != "team_pool" {
		t.Fatalf("pools %#v %v", poolList, diags)
	}

	summary := &privateWorkersSummaryDataSource{client: client}
	summaryRead := newDataRead(dataSchema(t, summary))
	summary.Read(ctx, summaryRead.req, summaryRead.resp)
	if summaryRead.resp.Diagnostics.HasError() {
		t.Fatal(summaryRead.resp.Diagnostics)
	}
	var counts privateWorkersSummaryModel
	if diags := summaryRead.resp.State.Get(ctx, &counts); diags.HasError() || counts.TeamTotalConnected.ValueInt64() != 2 || counts.TeamInUse.ValueInt64() != 1 {
		t.Fatalf("summary %#v %v", counts, diags)
	}
}

func TestPrivateWorkerPoolsOmittedScopeStaysNull(t *testing.T) {
	var gotScope string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotScope = r.URL.Query().Get("scope")
		_, _ = io.WriteString(w, `{"pools":[{"scope":"team","poolName":"gpu","workerReadyTimeoutSeconds":0,"connectedWorkerCount":1,"inUseWorkerCount":0,"isStale":false},{"scope":"user","poolName":"cpu","workerReadyTimeoutSeconds":0,"connectedWorkerCount":0,"inUseWorkerCount":0,"isStale":false}]}`)
	}))
	defer server.Close()

	ctx := context.Background()
	ds := &privateWorkerPoolsDataSource{client: publicClient(server.URL, "key_pool")}
	sch := dataSchema(t, ds)
	attr, ok := sch.Attributes["scope"].(dschema.StringAttribute)
	if !ok || !attr.Optional || attr.Computed {
		t.Fatalf("scope = %#v, want an optional non-computed string", sch.Attributes["scope"])
	}

	read := newDataRead(sch)
	read.req.Config = tfsdk.Config{Schema: sch, Raw: mustDataState(t, ctx, sch, &privateWorkerPoolsModel{Scope: types.StringNull()}).Raw}
	ds.Read(ctx, read.req, read.resp)
	if read.resp.Diagnostics.HasError() {
		t.Fatal(read.resp.Diagnostics)
	}
	var got privateWorkerPoolsModel
	if diags := read.resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if gotScope != "all" || !got.Scope.IsNull() || got.ID.ValueString() != "all" || len(got.Pools) != 2 {
		t.Fatalf("query scope=%q state scope null=%t id=%s pools=%d", gotScope, got.Scope.IsNull(), got.ID.ValueString(), len(got.Pools))
	}
}

func publicClient(base, key string) *apiClient {
	httpClient := &http.Client{}
	return &apiClient{
		httpClient: httpClient,
		public:     publicapi.New(publicapi.Options{BaseURL: base, APIKey: key, UserAgent: "terraform-provider-cursor/test+braze", HTTP: httpClient}),
		pools:      publicapi.New(publicapi.Options{BaseURL: base, APIKey: key, UserAgent: "terraform-provider-cursor/test+braze", HTTP: httpClient}),
	}
}

func poolClient(base, key string) *apiClient {
	return publicClient(base, key)
}

func resourceSchema(t *testing.T, res resource.Resource) schema.Schema {
	t.Helper()
	resp := &resource.SchemaResponse{}
	res.Schema(context.Background(), resource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.Schema
}

func mustPlan(t *testing.T, ctx context.Context, sch schema.Schema, model any) tfsdk.Plan {
	t.Helper()
	plan := tfsdk.Plan{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if diags := plan.Set(ctx, model); diags.HasError() {
		t.Fatal(diags)
	}
	return plan
}

func mustConfig(t *testing.T, ctx context.Context, sch schema.Schema, model any) tfsdk.Config {
	t.Helper()
	state := mustState(t, ctx, sch, model)
	return tfsdk.Config{Schema: sch, Raw: state.Raw}
}

func mustState(t *testing.T, ctx context.Context, sch schema.Schema, model any) tfsdk.State {
	t.Helper()
	state := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if diags := state.Set(ctx, model); diags.HasError() {
		t.Fatal(diags)
	}
	return state
}

func mustApply(t *testing.T, ctx context.Context, sch schema.Schema, fn func(*resource.CreateResponse)) tfsdk.State {
	t.Helper()
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	fn(resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.State
}

func mustRepos(t *testing.T, list types.List) []environmentRepoModel {
	t.Helper()
	var repos []environmentRepoModel
	if diags := list.ElementsAs(context.Background(), &repos, false); diags.HasError() {
		t.Fatal(diags)
	}
	return repos
}

func fmtDiag(d diag.Diagnostics) string {
	var b strings.Builder
	for _, err := range d.Errors() {
		b.WriteString(err.Summary())
		b.WriteString(" ")
		b.WriteString(err.Detail())
		b.WriteString("\n")
	}
	return b.String()
}

func schWait(sch schema.Schema) schema.Schema { return sch }

func dataSchema(t *testing.T, ds datasource.DataSource) dschema.Schema {
	t.Helper()
	resp := &datasource.SchemaResponse{}
	ds.Schema(context.Background(), datasource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.Schema
}

type dataRead struct {
	req  datasource.ReadRequest
	resp *datasource.ReadResponse
}

func mustDataState(t *testing.T, ctx context.Context, sch dschema.Schema, model any) tfsdk.State {
	t.Helper()
	state := tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	if diags := state.Set(ctx, model); diags.HasError() {
		t.Fatal(diags)
	}
	return state
}

func newDataRead(sch dschema.Schema) dataRead {
	return dataRead{
		resp: &datasource.ReadResponse{State: tfsdk.State{Schema: sch}},
	}
}
