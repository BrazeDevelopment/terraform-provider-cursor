package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Terraform rejects a plan whose value for a non-computed attribute differs from configuration,
// so fork resources may only use plan modifiers that force replacement on those attributes.
func TestForkSchemasDoNotRewriteConfiguredValues(t *testing.T) {
	ctx := context.Background()
	for _, factory := range []func() resource.Resource{
		NewEnvironmentResource,
		NewEnvironmentSecretResource,
		NewTeamSecretResource,
		NewPrivateWorkerPoolResource,
		NewModelAccessConfigurationResource,
		NewModelAccessProviderResource,
		NewModelAccessModelResource,
		NewUserSpendLimitResource,
		NewTeamDirectoryGroupResource,
		NewTeamDirectoryGroupMembersResource,
		NewOrganizationGroupResource,
		NewOrganizationGroupMembersResource,
		NewRepoBlocklistResource,
	} {
		res := factory()
		meta := &resource.MetadataResponse{}
		res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "cursor"}, meta)
		for name, attribute := range resourceSchema(t, res).Attributes {
			if attribute.IsComputed() {
				continue
			}
			for _, description := range planModifierDescriptions(ctx, attribute) {
				if !strings.Contains(description, "destroy and recreate") {
					t.Errorf("%s.%s is not computed and has plan modifier %q", meta.TypeName, name, description)
				}
			}
		}
	}
}

func planModifierDescriptions(ctx context.Context, attribute schema.Attribute) []string {
	var out []string
	switch a := attribute.(type) {
	case schema.StringAttribute:
		for _, m := range a.PlanModifiers {
			out = append(out, m.Description(ctx))
		}
	case schema.Int64Attribute:
		for _, m := range a.PlanModifiers {
			out = append(out, m.Description(ctx))
		}
	case schema.BoolAttribute:
		for _, m := range a.PlanModifiers {
			out = append(out, m.Description(ctx))
		}
	case schema.ListAttribute:
		for _, m := range a.PlanModifiers {
			out = append(out, m.Description(ctx))
		}
	case schema.SetAttribute:
		for _, m := range a.PlanModifiers {
			out = append(out, m.Description(ctx))
		}
	case schema.ListNestedAttribute:
		for _, m := range a.PlanModifiers {
			out = append(out, m.Description(ctx))
		}
	}
	return out
}

func TestSecretStateKeepsConfiguredRepoSpelling(t *testing.T) {
	ctx := context.Background()
	apiRepos := []string{"github.com/acme/api"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		item := map[string]any{"id": "secv_3qKx9mT2bV7nR1cW5yZ8aQ", "name": "NPM_TOKEN", "type": "runtime_secret", "repos": apiRepos}
		if r.Method == http.MethodPut {
			_ = json.NewEncoder(w).Encode(item)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{item}})
	}))
	defer server.Close()
	res := &secretResource{client: publicClient(server.URL, "key_test"), scope: secretScopeTeam}
	sch := resourceSchema(t, res)
	configured, _ := types.ListValueFrom(ctx, types.StringType, []string{"https://github.com/Acme/api.git", "acme/api"})
	plan := teamSecretModel{Name: types.StringValue("NPM_TOKEN"), Type: types.StringValue("runtime_secret"), Repos: configured, ValueWOVersion: types.Int64Value(1), ValueWO: types.StringNull(), ID: types.StringUnknown(), SecretVersionID: types.StringUnknown()}
	cfg := plan
	cfg.ValueWO = types.StringValue("value")
	create := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan), Config: mustConfig(t, ctx, sch, &cfg)}, create)
	if create.Diagnostics.HasError() {
		t.Fatal(create.Diagnostics)
	}
	var got teamSecretModel
	create.State.Get(ctx, &got)
	if !got.Repos.Equal(configured) {
		t.Fatalf("state repos = %v, want configured %v", got.Repos, configured)
	}

	apiRepos = []string{"github.com/acme/api", "github.com/acme/web"}
	read := &resource.ReadResponse{State: create.State}
	res.Read(ctx, resource.ReadRequest{State: create.State}, read)
	read.State.Get(ctx, &got)
	want, _ := types.ListValueFrom(ctx, types.StringType, apiRepos)
	if !got.Repos.Equal(want) {
		t.Fatalf("drifted repos = %v, want %v", got.Repos, want)
	}
}

func TestEnvironmentStateKeepsConfiguredSpelling(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var patches []string
	env := map[string]any{
		"id": "11111111-1111-1111-1111-111111111111", "owner": "team", "name": "web",
		"repos":           []any{map[string]any{"url": "https://github.com/acme/api"}},
		"environmentJson": `{"install":"pnpm","start":"up"}`, "createdAt": "t", "updatedAt": "t",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPatch {
			mu.Lock()
			patches = append(patches, string(raw))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(env)
	}))
	defer server.Close()
	res := &environmentResource{client: publicClient(server.URL, "key_test")}
	sch := resourceSchema(t, res)
	plan := environmentModel{
		ID:              types.StringUnknown(),
		Owner:           types.StringValue("team"),
		Name:            types.StringValue("web"),
		Repos:           []environmentRepoModel{{URL: types.StringValue("github.com/acme/api")}},
		EnvironmentJSON: types.StringValue("{\n  \"start\": \"up\",\n  \"install\": \"pnpm\"\n}"),
	}
	state := mustApply(t, ctx, sch, func(resp *resource.CreateResponse) {
		res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	})
	var got environmentModel
	state.Get(ctx, &got)
	if !got.EnvironmentJSON.Equal(plan.EnvironmentJSON) || got.Repos[0].URL.ValueString() != "github.com/acme/api" {
		t.Fatalf("state = %#v", got)
	}

	plan.ID = got.ID
	plan.EnvironmentJSON = types.StringValue(`{"start": "up", "install": "pnpm"}`)
	update := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: state}, update)
	if update.Diagnostics.HasError() {
		t.Fatal(update.Diagnostics)
	}
	if len(patches) != 0 {
		t.Fatalf("reformatted JSON should not PATCH: %v", patches)
	}
	update.State.Get(ctx, &got)
	if !got.EnvironmentJSON.Equal(plan.EnvironmentJSON) {
		t.Fatalf("state json = %s", got.EnvironmentJSON)
	}

	env["environmentJson"] = `{"install":"npm"}`
	read := &resource.ReadResponse{State: update.State}
	res.Read(ctx, resource.ReadRequest{State: update.State}, read)
	read.State.Get(ctx, &got)
	if got.EnvironmentJSON.ValueString() != `{"install":"npm"}` {
		t.Fatalf("drifted json = %s", got.EnvironmentJSON)
	}
}

func TestPoolStateKeepsConfiguredRepoURL(t *testing.T) {
	ctx := context.Background()
	apiURL := "https://github.com/acme/api"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"registered":true}`)
			return
		}
		fmt.Fprintf(w, `{"pools":[{"scope":"team","poolName":"gpu","repoOwner":"acme","repoName":"api","repoUrl":%q,"workerReadyTimeoutSeconds":0,"connectedWorkerCount":1,"inUseWorkerCount":0,"isStale":false}]}`, apiURL)
	}))
	defer server.Close()
	res := &privateWorkerPoolResource{client: poolClient(server.URL, "key_pool")}
	sch := resourceSchema(t, res)
	attr, ok := sch.Attributes["repo_url"].(schema.StringAttribute)
	if !ok || !attr.Optional || attr.Computed {
		t.Fatalf("repo_url = %#v, want an optional non-computed string", sch.Attributes["repo_url"])
	}

	omitted := privateWorkerPoolModel{
		Scope: types.StringValue("team"), PoolName: types.StringValue("gpu"),
		RepoOwner: types.StringValue("acme"), RepoName: types.StringValue("api"), RepoURL: types.StringNull(),
		WorkerReadyTimeoutSeconds: types.Int64Value(0),
	}
	state := mustApply(t, ctx, sch, func(resp *resource.CreateResponse) {
		res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &omitted)}, resp)
	})
	var got privateWorkerPoolModel
	state.Get(ctx, &got)
	if !got.RepoURL.IsNull() || got.ID.ValueString() != "team/gpu/acme/api" {
		t.Fatalf("omitted repo_url state = %#v", got)
	}

	configured := "https://github.com/Acme/api.git"
	plan := omitted
	plan.RepoURL = types.StringValue(configured)
	state = mustApply(t, ctx, sch, func(resp *resource.CreateResponse) {
		res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	})
	state.Get(ctx, &got)
	if got.RepoURL.ValueString() != configured {
		t.Fatalf("state repo_url = %s, want configured %s", got.RepoURL, configured)
	}

	apiURL = ""
	read := &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, read)
	if read.Diagnostics.HasError() {
		t.Fatal(read.Diagnostics)
	}
	read.State.Get(ctx, &got)
	if got.RepoURL.ValueString() != configured {
		t.Fatalf("empty api repo_url cleared state to %s", got.RepoURL)
	}

	apiURL = "https://github.com/acme/other"
	read = &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, read)
	if read.Diagnostics.HasError() {
		t.Fatal(read.Diagnostics)
	}
	read.State.Get(ctx, &got)
	if got.RepoURL.ValueString() != apiURL {
		t.Fatalf("drifted repo_url = %s, want %s", got.RepoURL, apiURL)
	}
}

func TestGuardrailsTreatUnsetScopeAsUserAndCheckEveryRule(t *testing.T) {
	ctx := context.Background()
	res := &platformWorkflowResource{client: &apiClient{guardrails: &guardrailsConfig{
		HasAllowedScopes: true,
		AllowedScopes:    []string{"team"},
		RequirePoolLabel: true,
		HasAllowedModels: true,
		AllowedModels:    []string{"composer-2"},
	}}}
	sch := resourceSchema(t, res)
	cfg := platformWorkflowModel{
		Name:                 types.StringValue("review"),
		Prompt:               types.StringValue("review"),
		Scope:                types.StringNull(),
		Model:                types.StringValue("gpt-4o"),
		DisabledDefaultTools: types.ListNull(types.StringType),
	}
	plan := cfg
	plan.Scope = types.StringUnknown()
	resp := &resource.ModifyPlanResponse{Plan: mustPlan(t, ctx, sch, &plan)}
	res.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: resp.Plan, Config: mustConfig(t, ctx, sch, &cfg)}, resp)
	text := fmtDiag(resp.Diagnostics)
	for _, needle := range []string{`scope "user"`, "private_worker.labels.pool", `model "gpt-4o"`} {
		if !strings.Contains(text, needle) {
			t.Fatalf("diagnostics missing %s: %s", needle, text)
		}
	}
}

func TestGuardrailsOmittedScopeUsesPlanValue(t *testing.T) {
	ctx := context.Background()
	sch := resourceSchema(t, &platformWorkflowResource{})
	cfg := platformWorkflowModel{
		Name:                 types.StringValue("review"),
		Prompt:               types.StringValue("review"),
		Scope:                types.StringNull(),
		DisabledDefaultTools: types.ListNull(types.StringType),
	}
	for _, scope := range []string{"team", "team_visible", "team_editable", "team_editable_user"} {
		plan := cfg
		plan.Scope = types.StringValue(scope)

		reject := &platformWorkflowResource{client: &apiClient{guardrails: &guardrailsConfig{
			HasAllowedScopes: true,
			AllowedScopes:    []string{"user"},
		}}}
		resp := &resource.ModifyPlanResponse{Plan: mustPlan(t, ctx, sch, &plan)}
		reject.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: resp.Plan, Config: mustConfig(t, ctx, sch, &cfg)}, resp)
		text := fmtDiag(resp.Diagnostics)
		if !strings.Contains(text, fmt.Sprintf("scope %q", scope)) || strings.Contains(text, `scope "user"`) {
			t.Fatalf("omitted scope should keep prior %s value: %s", scope, text)
		}

		allow := &platformWorkflowResource{client: &apiClient{guardrails: &guardrailsConfig{
			HasAllowedScopes: true,
			AllowedScopes:    []string{scope},
		}}}
		resp = &resource.ModifyPlanResponse{Plan: mustPlan(t, ctx, sch, &plan)}
		allow.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: resp.Plan, Config: mustConfig(t, ctx, sch, &cfg)}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("prior %s scope should satisfy its allow-list: %s", scope, fmtDiag(resp.Diagnostics))
		}
	}
}

func TestGuardrailsUnknownRuleDoesNotSkipOthers(t *testing.T) {
	ctx := context.Background()
	res := &platformWorkflowResource{client: &apiClient{guardrails: &guardrailsConfig{
		HasAllowedScopes: true,
		AllowedScopes:    []string{"team_visible"},
		RequirePoolLabel: true,
	}}}
	sch := resourceSchema(t, res)
	cfg := platformWorkflowModel{
		Name:                 types.StringValue("review"),
		Prompt:               types.StringValue("review"),
		Scope:                types.StringUnknown(),
		DisabledDefaultTools: types.ListNull(types.StringType),
	}
	plan := cfg
	plan.Scope = types.StringValue("team")
	resp := &resource.ModifyPlanResponse{Plan: mustPlan(t, ctx, sch, &plan)}
	res.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: resp.Plan, Config: mustConfig(t, ctx, sch, &cfg)}, resp)
	text := fmtDiag(resp.Diagnostics)
	if strings.Contains(text, "scope") || !strings.Contains(text, "private_worker") {
		t.Fatalf("unknown scope should wait and the pool rule should still fail: %s", text)
	}
}

func TestModelParametersKeepNullAllowedValues(t *testing.T) {
	ctx := context.Background()
	empty, _ := types.ListValueFrom(ctx, types.StringType, []string{})
	prior := []modelParameterModel{
		{ID: types.StringValue("fast"), AllowedValues: types.ListNull(types.StringType), DefaultValue: types.StringNull()},
		{ID: types.StringValue("reasoning"), AllowedValues: empty, DefaultValue: types.StringNull()},
	}
	got := parametersFromAPI(ctx, prior, []modelAccessParameterRecord{
		{ID: "fast", AllowedValues: nil},
		{ID: "reasoning", AllowedValues: []string{}},
	})
	if !got[0].AllowedValues.IsNull() {
		t.Fatalf("null restriction became %v", got[0].AllowedValues)
	}
	if got[1].AllowedValues.IsNull() || len(got[1].AllowedValues.Elements()) != 0 {
		t.Fatalf("empty restriction became %v", got[1].AllowedValues)
	}
}

func TestGroupMembersAreASet(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	res := &groupMembersResource{client: &apiClient{httpClient: server.Client(), adminBase: server.URL, teamAPIKey: "team-key", userAgent: "test"}, kind: groupKindTeam}
	sch := resourceSchema(t, res)
	ids, _ := types.SetValueFrom(ctx, types.StringType, []string{"user_b", "user_a"})
	plan := groupMembersModel{ID: types.StringUnknown(), GroupID: types.StringValue("team_group_1"), UserIDs: ids}
	state := mustApply(t, ctx, sch, func(resp *resource.CreateResponse) {
		res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	})
	var got groupMembersModel
	state.Get(ctx, &got)
	if !got.UserIDs.Equal(ids) {
		t.Fatalf("user_ids = %v, want %v", got.UserIDs, ids)
	}
}

func TestGroupMembersRefreshDedupesPagedUserIDs(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		hasNext := page == "1"
		members := []groupMemberRecord{{UserID: "user_a"}, {UserID: "user_a"}, {UserID: "user_b"}}
		if page == "2" {
			members = []groupMemberRecord{{UserID: "user_b"}, {UserID: "user_c"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"members":    members,
			"pagination": map[string]any{"hasNextPage": hasNext},
		})
	}))
	defer server.Close()
	res := &groupMembersResource{client: &apiClient{httpClient: server.Client(), adminBase: server.URL, teamAPIKey: "team-key", userAgent: "test"}, kind: groupKindTeam}
	sch := resourceSchema(t, res)
	priorIDs, diags := types.SetValueFrom(ctx, types.StringType, []string{"user_a"})
	if diags.HasError() {
		t.Fatal(diags)
	}
	prior := groupMembersModel{ID: types.StringValue("team_group_1"), GroupID: types.StringValue("team_group_1"), UserIDs: priorIDs}
	state := mustState(t, ctx, sch, &prior)
	read := &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, read)
	if read.Diagnostics.HasError() {
		t.Fatal(read.Diagnostics)
	}
	var got groupMembersModel
	if diags := read.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	want, diags := types.SetValueFrom(ctx, types.StringType, []string{"user_a", "user_b", "user_c"})
	if diags.HasError() {
		t.Fatal(diags)
	}
	if got.UserIDs.IsNull() || got.UserIDs.IsUnknown() || !got.UserIDs.Equal(want) {
		t.Fatalf("user_ids = %v, want %v", got.UserIDs, want)
	}
}

func TestGroupMembersRefreshKeepsEmptySet(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"members":[],"pagination":{"hasNextPage":false}}`)
	}))
	defer server.Close()
	res := &groupMembersResource{client: &apiClient{httpClient: server.Client(), adminBase: server.URL, teamAPIKey: "team-key", userAgent: "test"}, kind: groupKindTeam}
	sch := resourceSchema(t, res)
	priorIDs, diags := types.SetValueFrom(ctx, types.StringType, []string{"user_a"})
	if diags.HasError() {
		t.Fatal(diags)
	}
	prior := groupMembersModel{ID: types.StringValue("team_group_1"), GroupID: types.StringValue("team_group_1"), UserIDs: priorIDs}
	state := mustState(t, ctx, sch, &prior)
	read := &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, read)
	if read.Diagnostics.HasError() {
		t.Fatal(read.Diagnostics)
	}
	var got groupMembersModel
	if diags := read.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.UserIDs.IsNull() || got.UserIDs.IsUnknown() || len(got.UserIDs.Elements()) != 0 {
		t.Fatalf("user_ids = %v, want empty set", got.UserIDs)
	}
}

func TestAdminResourcesImport(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		res   resource.ResourceWithImportState
		id    string
		attrs map[string]string
	}{
		{&modelAccessModelResource{}, "anthropic/claude-opus-4-6", map[string]string{"provider_id": "anthropic", "model_id": "claude-opus-4-6"}},
		{&modelAccessProviderResource{}, "anthropic", map[string]string{"provider_id": "anthropic"}},
		{&userSpendLimitResource{}, "a@acme.com", map[string]string{"user_email": "a@acme.com"}},
		{&repoBlocklistResource{}, "repo_1", map[string]string{"id": "repo_1"}},
		{&groupResource{kind: groupKindOrganization}, "g_1", map[string]string{"id": "g_1"}},
		{&groupMembersResource{kind: groupKindTeam}, "team_group_1", map[string]string{"id": "team_group_1", "group_id": "team_group_1"}},
	}
	for _, tc := range cases {
		sch := resourceSchema(t, tc.res)
		resp := &resource.ImportStateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}}
		tc.res.ImportState(ctx, resource.ImportStateRequest{ID: tc.id}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: %v", tc.id, resp.Diagnostics)
		}
		for name, want := range tc.attrs {
			var got types.String
			resp.State.GetAttribute(ctx, path.Root(name), &got)
			if got.ValueString() != want {
				t.Fatalf("%s: %s = %q, want %q", tc.id, name, got.ValueString(), want)
			}
		}
	}

	bad := &resource.ImportStateResponse{}
	(&modelAccessModelResource{}).ImportState(ctx, resource.ImportStateRequest{ID: "claude-opus-4-6"}, bad)
	if !bad.Diagnostics.HasError() {
		t.Fatal("model import without provider should fail")
	}
}
