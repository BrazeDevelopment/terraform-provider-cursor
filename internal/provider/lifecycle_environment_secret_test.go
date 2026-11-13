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

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
)

type lesCall struct {
	Method string
	Target string
	Body   string
}

type lesServer struct {
	mu    sync.Mutex
	calls []lesCall
}

// lesClient serves each public API request from respond, which returns a status and a JSON body, and records every call with its query string.
func lesClient(t *testing.T, respond func(call lesCall) (int, string)) (*apiClient, *lesServer) {
	t.Helper()
	rec := &lesServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := lesCall{Method: r.Method, Target: r.URL.RequestURI(), Body: string(raw)}
		rec.mu.Lock()
		rec.calls = append(rec.calls, call)
		rec.mu.Unlock()
		status, body := respond(call)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return publicClient(server.URL, "key_test"), rec
}

func (s *lesServer) with(method string) []lesCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []lesCall
	for _, call := range s.calls {
		if call.Method == method {
			out = append(out, call)
		}
	}
	return out
}

func lesJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

const lesEnvID = "11111111-1111-1111-1111-111111111111"

func lesEnvironment(urls ...string) environmentModel {
	repos := make([]environmentRepoModel, 0, len(urls))
	for _, url := range urls {
		repos = append(repos, environmentRepoModel{URL: types.StringValue(url)})
	}
	return environmentModel{
		ID:              types.StringValue(lesEnvID),
		Owner:           types.StringValue("team"),
		Name:            types.StringValue("web"),
		Repos:           repos,
		EnvironmentJSON: types.StringValue(`{"install":"pnpm"}`),
	}
}

func lesEnvironmentBody(t *testing.T, name, environmentJSON string, urls ...string) map[string]any {
	t.Helper()
	repos := make([]any, 0, len(urls))
	for _, url := range urls {
		repos = append(repos, map[string]any{"url": url})
	}
	body := map[string]any{"id": lesEnvID, "owner": "team", "repos": repos, "createdAt": "t", "updatedAt": "t"}
	if name != "" {
		body["name"] = name
	}
	if environmentJSON != "" {
		body["environmentJson"] = environmentJSON
	}
	return body
}

func lesWarnings(d diag.Diagnostics) string {
	var b strings.Builder
	for _, w := range d.Warnings() {
		b.WriteString(w.Summary() + " " + w.Detail() + "\n")
	}
	return b.String()
}

func TestEnvironmentModifyPlanChecksKnownReposBesideUnknown(t *testing.T) {
	ctx := context.Background()
	guarded := &environmentResource{client: &apiClient{guardrails: &guardrailsConfig{HasAllowedOwners: true, AllowedOwners: []string{"acme"}}}}
	sch := resourceSchema(t, guarded)
	model := lesEnvironment("https://github.com/acme/api", "https://github.com/evil/tool", "git@github.com:ACME/web.git", "github.com")
	modify := func(res *environmentResource, raw tftypes.Value) diag.Diagnostics {
		plan := tfsdk.Plan{Schema: sch, Raw: raw}
		resp := &resource.ModifyPlanResponse{Plan: plan}
		res.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, Config: tfsdk.Config{Schema: sch, Raw: raw}, State: emptyState(ctx, sch)}, resp)
		return resp.Diagnostics
	}
	known := mustState(t, ctx, sch, &model).Raw

	lpgWantPaths(t, modify(guarded, known), "repos[1].url", "repos[3].url")

	oneUnknown := lpgUnknown(t, ctx, sch, &model, path.Root("repos").AtListIndex(0).AtName("url"))
	lpgWantPaths(t, modify(guarded, oneUnknown), "repos[1].url", "repos[3].url")

	violationUnknown := lpgUnknown(t, ctx, sch, &model, path.Root("repos").AtListIndex(1).AtName("url"), path.Root("repos").AtListIndex(3).AtName("url"))
	if diags := modify(guarded, violationUnknown); diags.HasError() {
		t.Errorf("unknown urls should wait for apply: %s", fmtDiag(diags))
	}

	for name, raw := range map[string]tftypes.Value{
		"unknown element": lpgUnknown(t, ctx, sch, &model, path.Root("repos").AtListIndex(1)),
		"unknown list":    lpgUnknown(t, ctx, sch, &model, path.Root("repos")),
		"destroy":         tftypes.NewValue(sch.Type().TerraformType(ctx), nil),
	} {
		diags := modify(guarded, raw)
		if name == "unknown element" {
			lpgWantPaths(t, diags, "repos[3].url")
			continue
		}
		if diags.HasError() {
			t.Errorf("%s: %s", name, fmtDiag(diags))
		}
	}

	for name, res := range map[string]*environmentResource{
		"nil client":      {},
		"no guardrails":   {client: &apiClient{}},
		"no owner allow":  {client: &apiClient{guardrails: &guardrailsConfig{}}},
		"owner is listed": {client: &apiClient{guardrails: &guardrailsConfig{HasAllowedOwners: true, AllowedOwners: []string{"Acme", "evil"}}}},
	} {
		diags := modify(res, known)
		if name == "owner is listed" {
			lpgWantPaths(t, diags, "repos[3].url")
			continue
		}
		if diags.HasError() {
			t.Errorf("%s: %s", name, fmtDiag(diags))
		}
	}
}

func TestEnvironmentReposReplaceOnlyWhenRepositoriesChange(t *testing.T) {
	ctx := context.Background()
	sch := resourceSchema(t, &environmentResource{})
	modifiers := sch.Attributes["repos"].(schema.ListNestedAttribute).PlanModifiers
	if len(modifiers) != 1 {
		t.Fatalf("repos plan modifiers = %d", len(modifiers))
	}
	prior := lesEnvironment("https://github.com/acme/api", "https://github.com/acme/web")
	state := mustState(t, ctx, sch, &prior)
	replaces := func(raw tftypes.Value) bool {
		t.Helper()
		plan := tfsdk.Plan{Schema: sch, Raw: raw}
		var stateValue, planValue types.List
		if diags := state.GetAttribute(ctx, path.Root("repos"), &stateValue); diags.HasError() {
			t.Fatal(diags)
		}
		if diags := plan.GetAttribute(ctx, path.Root("repos"), &planValue); diags.HasError() {
			t.Fatal(diags)
		}
		resp := &planmodifier.ListResponse{PlanValue: planValue}
		modifiers[0].PlanModifyList(ctx, planmodifier.ListRequest{Path: path.Root("repos"), State: state, Plan: plan, StateValue: stateValue, PlanValue: planValue, ConfigValue: planValue}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatal(resp.Diagnostics)
		}
		if !resp.PlanValue.Equal(planValue) {
			t.Errorf("the modifier changed the planned repos: %v", resp.PlanValue)
		}
		return resp.RequiresReplace
	}
	raw := func(urls ...string) tftypes.Value {
		model := lesEnvironment(urls...)
		return mustState(t, ctx, sch, &model).Raw
	}
	for name, tc := range map[string]struct {
		plan    tftypes.Value
		replace bool
	}{
		"unchanged":       {raw("https://github.com/acme/api", "https://github.com/acme/web"), false},
		"respelled":       {raw("git@github.com:Acme/API.git", "acme/web"), false},
		"reordered":       {raw("https://github.com/acme/web", "https://github.com/acme/api"), true},
		"added":           {raw("https://github.com/acme/api", "https://github.com/acme/web", "https://github.com/acme/docs"), true},
		"removed":         {raw("https://github.com/acme/api"), true},
		"another repo":    {raw("https://github.com/acme/api", "https://github.com/acme/docs"), true},
		"unknown url":     {lpgUnknown(t, ctx, sch, &prior, path.Root("repos").AtListIndex(1).AtName("url")), true},
		"unknown element": {lpgUnknown(t, ctx, sch, &prior, path.Root("repos").AtListIndex(1)), true},
		"unknown list":    {lpgUnknown(t, ctx, sch, &prior, path.Root("repos")), true},
	} {
		if got := replaces(tc.plan); got != tc.replace {
			t.Errorf("%s: requires replace = %t, want %t", name, got, tc.replace)
		}
	}
}

func TestEnvironmentValidators(t *testing.T) {
	ctx := context.Background()
	sch := resourceSchema(t, &environmentResource{})

	jsonValidators := sch.Attributes["environment_json"].(schema.StringAttribute).Validators
	for value, valid := range map[string]bool{
		`{}`:                  true,
		` {"install": "x"} `:  true,
		`[]`:                  false,
		`"pnpm install"`:      false,
		`null`:                false,
		`{"install":`:         false,
		`{"a":1} {"b":2}`:     false,
		``:                    false,
		`{"install":"x"}junk`: false,
	} {
		errs := 0
		for _, v := range jsonValidators {
			resp := &validator.StringResponse{}
			v.ValidateString(ctx, validator.StringRequest{Path: path.Root("environment_json"), ConfigValue: types.StringValue(value)}, resp)
			errs += resp.Diagnostics.ErrorsCount()
		}
		if valid != (errs == 0) {
			t.Errorf("environment_json %q: errors = %d, want valid = %t", value, errs, valid)
		}
	}
	for _, v := range jsonValidators {
		resp := &validator.StringResponse{}
		v.ValidateString(ctx, validator.StringRequest{Path: path.Root("environment_json"), ConfigValue: types.StringUnknown()}, resp)
		if resp.Diagnostics.HasError() {
			t.Errorf("unknown environment_json should wait: %s", fmtDiag(resp.Diagnostics))
		}
	}

	nameValidators := sch.Attributes["name"].(schema.StringAttribute).Validators
	for value, valid := range map[string]bool{"web": true, "": false, strings.Repeat("é", 255): true, strings.Repeat("a", 256): false} {
		errs := 0
		for _, v := range nameValidators {
			resp := &validator.StringResponse{}
			v.ValidateString(ctx, validator.StringRequest{Path: path.Root("name"), ConfigValue: types.StringValue(value)}, resp)
			errs += resp.Diagnostics.ErrorsCount()
		}
		if valid != (errs == 0) {
			t.Errorf("name of %d bytes: errors = %d, want valid = %t", len(value), errs, valid)
		}
	}

	repoValidators := sch.Attributes["repos"].(schema.ListNestedAttribute).Validators
	validate := func(raw tftypes.Value) diag.Diagnostics {
		t.Helper()
		var repos types.List
		if diags := (tfsdk.Config{Schema: sch, Raw: raw}).GetAttribute(ctx, path.Root("repos"), &repos); diags.HasError() {
			t.Fatal(diags)
		}
		var out diag.Diagnostics
		for _, v := range repoValidators {
			resp := &validator.ListResponse{}
			v.ValidateList(ctx, validator.ListRequest{Path: path.Root("repos"), ConfigValue: repos}, resp)
			out.Append(resp.Diagnostics...)
		}
		return out
	}
	model := lesEnvironment("https://github.com/acme/api", "git@github.com:Acme/API.git", "  ", "acme/web")
	lpgWantPaths(t, validate(mustState(t, ctx, sch, &model).Raw), "repos[1].url", "repos[2].url")
	if diags := validate(lpgUnknown(t, ctx, sch, &model, path.Root("repos").AtListIndex(1).AtName("url"), path.Root("repos").AtListIndex(2).AtName("url"))); diags.HasError() {
		t.Errorf("unknown urls should wait: %s", fmtDiag(diags))
	}
	if diags := validate(lpgUnknown(t, ctx, sch, &model, path.Root("repos"))); diags.HasError() {
		t.Errorf("an unknown list should wait: %s", fmtDiag(diags))
	}
	empty := lesEnvironment()
	if diags := validate(mustState(t, ctx, sch, &empty).Raw); diags.HasError() {
		t.Errorf("an empty list is valid: %s", fmtDiag(diags))
	}
	var many []string
	for i := range 101 {
		many = append(many, "acme/repo-"+strings.Repeat("x", i+1))
	}
	tooMany := lesEnvironment(many...)
	if diags := validate(mustState(t, ctx, sch, &tooMany).Raw); diags.ErrorsCount() != 1 {
		t.Errorf("101 repositories: %s", fmtDiag(diags))
	}
}

func TestEnvironmentCreateKeepsPlannedValuesWhenAPIDiffers(t *testing.T) {
	ctx := context.Background()
	client, rec := lesClient(t, func(call lesCall) (int, string) {
		if call.Method != http.MethodPost {
			return http.StatusInternalServerError, `{"error":{"code":"x","message":"only the create call is expected"}}`
		}
		return http.StatusCreated, lesJSON(t, lesEnvironmentBody(t, "Web", "", "https://github.com/acme/renamed"))
	})
	res := &environmentResource{client: client}
	sch := resourceSchema(t, res)
	plan := lesEnvironment("github.com/acme/api")
	plan.ID = types.StringUnknown()
	plan.EnvironmentJSON = types.StringValue("{\n  \"install\": \"pnpm\"\n}")
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(fmtDiag(resp.Diagnostics))
	}
	var got environmentModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	want := plan
	want.ID = types.StringValue(lesEnvID)
	if !got.ID.Equal(want.ID) || !got.Name.Equal(want.Name) || !got.Owner.Equal(want.Owner) || !got.EnvironmentJSON.Equal(want.EnvironmentJSON) || len(got.Repos) != 1 || !got.Repos[0].URL.Equal(want.Repos[0].URL) {
		t.Fatalf("state = %#v, want the planned values with the API id", got)
	}
	posts := rec.with(http.MethodPost)
	if len(posts) != 1 || !strings.Contains(posts[0].Body, `"owner":"team"`) || !strings.Contains(posts[0].Body, `"name":"web"`) || !strings.Contains(posts[0].Body, `"repos":[{"url":"github.com/acme/api"}]`) || !strings.Contains(posts[0].Body, `"environmentJson":"{\n  \"install\": \"pnpm\"\n}"`) {
		t.Fatalf("create calls = %#v", posts)
	}
}

func TestEnvironmentEmptyReposStayAnEmptyList(t *testing.T) {
	ctx := context.Background()
	repos := "[]"
	client, _ := lesClient(t, func(call lesCall) (int, string) {
		status := http.StatusOK
		if call.Method == http.MethodPost {
			status = http.StatusCreated
		}
		return status, `{"id":"` + lesEnvID + `","owner":"team","name":"web","repos":` + repos + `,"environmentJson":"{\"install\":\"pnpm\"}","createdAt":"t","updatedAt":"t"}`
	})
	res := &environmentResource{client: client}
	sch := resourceSchema(t, res)
	plan := lesEnvironment()
	plan.ID = types.StringUnknown()
	wantEmpty := func(state tfsdk.State) {
		t.Helper()
		var list types.List
		if diags := state.GetAttribute(ctx, path.Root("repos"), &list); diags.HasError() {
			t.Fatal(diags)
		}
		if list.IsNull() || list.IsUnknown() || len(list.Elements()) != 0 {
			t.Fatalf("repos = %v, want an empty list", list)
		}
	}
	created := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, created)
	if created.Diagnostics.HasError() {
		t.Fatal(fmtDiag(created.Diagnostics))
	}
	wantEmpty(created.State)
	for _, repos = range []string{"[]", "null"} {
		read := &resource.ReadResponse{State: created.State}
		res.Read(ctx, resource.ReadRequest{State: created.State}, read)
		if read.Diagnostics.HasError() {
			t.Fatal(fmtDiag(read.Diagnostics))
		}
		wantEmpty(read.State)
		imported := &resource.ImportStateResponse{State: emptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: lesEnvID}, imported)
		read = &resource.ReadResponse{State: imported.State}
		res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
		if read.Diagnostics.HasError() {
			t.Fatal(fmtDiag(read.Diagnostics))
		}
		wantEmpty(read.State)
	}
}

func TestEnvironmentCreateWithoutIDRecordsNoState(t *testing.T) {
	ctx := context.Background()
	client, _ := lesClient(t, func(lesCall) (int, string) {
		return http.StatusCreated, `{"owner":"team","name":"web","repos":[],"createdAt":"t","updatedAt":"t"}`
	})
	res := &environmentResource{client: client}
	sch := resourceSchema(t, res)
	plan := lesEnvironment()
	plan.ID = types.StringUnknown()
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(fmtDiag(resp.Diagnostics), "import") {
		t.Fatalf("a create response without an id should fail and point at import: %s", fmtDiag(resp.Diagnostics))
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("no state should be recorded without an id: %v", resp.State.Raw)
	}
}

func TestEnvironmentUpdate(t *testing.T) {
	ctx := context.Background()
	type result struct {
		diags   diag.Diagnostics
		state   tfsdk.State
		patches []lesCall
	}
	// The fake keeps returning current, as an API whose reads lag its writes would.
	update := func(t *testing.T, current map[string]any, getStatus int, prior, plan environmentModel) result {
		t.Helper()
		client, rec := lesClient(t, func(call lesCall) (int, string) {
			switch call.Method {
			case http.MethodGet:
				if getStatus != 0 {
					return getStatus, `{"error":{"code":"environment_not_found","message":"missing"}}`
				}
				return http.StatusOK, lesJSON(t, current)
			case http.MethodPatch:
				return http.StatusNoContent, ""
			}
			return http.StatusInternalServerError, `{"error":{"code":"x","message":"unexpected"}}`
		})
		res := &environmentResource{client: client}
		sch := resourceSchema(t, res)
		priorState := mustState(t, ctx, sch, &prior)
		resp := &resource.UpdateResponse{State: priorState}
		res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: priorState}, resp)
		return result{diags: resp.Diagnostics, state: resp.State, patches: rec.with(http.MethodPatch)}
	}
	model := func(t *testing.T, state tfsdk.State) environmentModel {
		t.Helper()
		var got environmentModel
		if diags := state.Get(ctx, &got); diags.HasError() {
			t.Fatal(diags)
		}
		return got
	}

	t.Run("rename sends only name and keeps the plan in state", func(t *testing.T) {
		prior := lesEnvironment("https://github.com/acme/api")
		plan := prior
		plan.Name = types.StringValue("web-2")
		got := update(t, lesEnvironmentBody(t, "web", `{"install": "pnpm"}`, "https://github.com/acme/api"), 0, prior, plan)
		if got.diags.HasError() {
			t.Fatal(fmtDiag(got.diags))
		}
		if len(got.patches) != 1 || got.patches[0].Body != `{"name":"web-2"}` {
			t.Fatalf("patches = %#v", got.patches)
		}
		if state := model(t, got.state); state.Name.ValueString() != "web-2" || !state.EnvironmentJSON.Equal(plan.EnvironmentJSON) || state.ID.ValueString() != lesEnvID {
			t.Fatalf("state = %#v", state)
		}
	})

	t.Run("configuration change sends only environmentJson", func(t *testing.T) {
		prior := lesEnvironment("https://github.com/acme/api")
		plan := prior
		plan.EnvironmentJSON = types.StringValue(`{"start":"up","install":"pnpm"}`)
		got := update(t, lesEnvironmentBody(t, "web", `{"install":"pnpm"}`, "https://github.com/acme/api"), 0, prior, plan)
		if got.diags.HasError() {
			t.Fatal(fmtDiag(got.diags))
		}
		if len(got.patches) != 1 || strings.Contains(got.patches[0].Body, `"name"`) || !strings.Contains(got.patches[0].Body, `"environmentJson"`) {
			t.Fatalf("patches = %#v", got.patches)
		}
		if state := model(t, got.state); !state.EnvironmentJSON.Equal(plan.EnvironmentJSON) {
			t.Fatalf("state json = %s", state.EnvironmentJSON)
		}
	})

	t.Run("respelled repos update state without a write", func(t *testing.T) {
		prior := lesEnvironment("https://github.com/acme/api")
		plan := lesEnvironment("git@github.com:acme/api.git")
		current := lesEnvironmentBody(t, "web", `{"install":"pnpm"}`, "https://github.com/acme/api")
		current["repoFile"] = map[string]any{"url": "https://github.com/acme/api", "path": ".cursor/environment.json"}
		got := update(t, current, 0, prior, plan)
		if got.diags.HasError() {
			t.Fatal(fmtDiag(got.diags))
		}
		if len(got.patches) != 0 {
			t.Fatalf("patches = %#v", got.patches)
		}
		if state := model(t, got.state); len(state.Repos) != 1 || state.Repos[0].URL.ValueString() != "git@github.com:acme/api.git" {
			t.Fatalf("state repos = %#v", state.Repos)
		}
	})

	t.Run("repositories changed outside Terraform", func(t *testing.T) {
		prior := lesEnvironment("https://github.com/acme/api")
		plan := prior
		plan.Name = types.StringValue("web-2")
		got := update(t, lesEnvironmentBody(t, "web", `{"install":"pnpm"}`, "https://github.com/acme/other"), 0, prior, plan)
		lpgWantPaths(t, got.diags, "repos")
		if len(got.patches) != 0 {
			t.Fatalf("patches = %#v", got.patches)
		}
		if state := model(t, got.state); state.Name.ValueString() != "web" {
			t.Fatalf("a failed update must keep the prior state: %#v", state)
		}
	})

	t.Run("deleted outside Terraform", func(t *testing.T) {
		prior := lesEnvironment("https://github.com/acme/api")
		plan := prior
		plan.Name = types.StringValue("web-2")
		got := update(t, nil, http.StatusNotFound, prior, plan)
		if !got.diags.HasError() || !strings.Contains(fmtDiag(got.diags), "no longer exists") {
			t.Fatalf("diagnostics = %s", fmtDiag(got.diags))
		}
		if got.state.Raw.IsNull() {
			t.Fatal("an update must not return an empty state")
		}
		if len(got.patches) != 0 {
			t.Fatalf("patches = %#v", got.patches)
		}
	})

	t.Run("name drifted while refresh was skipped", func(t *testing.T) {
		prior := lesEnvironment("https://github.com/acme/api")
		plan := prior
		plan.EnvironmentJSON = types.StringValue(`{"install":"npm"}`)
		got := update(t, lesEnvironmentBody(t, "renamed", `{"install":"pnpm"}`, "https://github.com/acme/api"), 0, prior, plan)
		if got.diags.HasError() {
			t.Fatal(fmtDiag(got.diags))
		}
		if len(got.patches) != 1 || !strings.Contains(got.patches[0].Body, `"name":"web"`) || !strings.Contains(got.patches[0].Body, `"environmentJson"`) {
			t.Fatalf("patches = %#v", got.patches)
		}
	})
}

func TestEnvironmentImportAndDelete(t *testing.T) {
	ctx := context.Background()
	status := http.StatusOK
	client, rec := lesClient(t, func(call lesCall) (int, string) {
		if status != http.StatusOK {
			return status, `{"error":{"code":"environment_not_found","message":"missing"}}`
		}
		return http.StatusOK, lesJSON(t, lesEnvironmentBody(t, "web", "{ \"start\": \"up\",\n \"install\": \"pnpm\" }", "https://github.com/acme/api", "https://github.com/acme/web"))
	})
	res := &environmentResource{client: client}
	sch := resourceSchema(t, res)

	for _, id := range []string{"", "   ", "a/b", "two words"} {
		resp := &resource.ImportStateResponse{State: emptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: id}, resp)
		if !resp.Diagnostics.HasError() || !strings.Contains(fmtDiag(resp.Diagnostics), "UUID") {
			t.Errorf("import id %q: %s", id, fmtDiag(resp.Diagnostics))
		}
	}

	imported := &resource.ImportStateResponse{State: emptyState(ctx, sch)}
	res.ImportState(ctx, resource.ImportStateRequest{ID: " " + lesEnvID + "\n"}, imported)
	if imported.Diagnostics.HasError() {
		t.Fatal(fmtDiag(imported.Diagnostics))
	}
	read := &resource.ReadResponse{State: imported.State}
	res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
	if read.Diagnostics.HasError() {
		t.Fatal(fmtDiag(read.Diagnostics))
	}
	if gets := rec.with(http.MethodGet); len(gets) != 1 || gets[0].Target != "/v1/environments/"+lesEnvID {
		t.Fatalf("read calls = %#v", gets)
	}
	var got environmentModel
	if diags := read.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.ID.ValueString() != lesEnvID || got.Owner.ValueString() != "team" || got.Name.ValueString() != "web" || got.EnvironmentJSON.ValueString() != `{"install":"pnpm","start":"up"}` || len(got.Repos) != 2 || got.Repos[1].URL.ValueString() != "https://github.com/acme/web" {
		t.Fatalf("imported state = %#v", got)
	}

	// A configuration that spells the imported values differently plans no replacement and no write.
	config := lesEnvironment("acme/api", "github.com/acme/web.git")
	config.EnvironmentJSON = types.StringValue(`{"install":"pnpm","start":"up"}`)
	var stateRepos, planRepos types.List
	plan := mustPlan(t, ctx, sch, &config)
	read.State.GetAttribute(ctx, path.Root("repos"), &stateRepos)
	plan.GetAttribute(ctx, path.Root("repos"), &planRepos)
	modified := &planmodifier.ListResponse{PlanValue: planRepos}
	sch.Attributes["repos"].(schema.ListNestedAttribute).PlanModifiers[0].PlanModifyList(ctx, planmodifier.ListRequest{Path: path.Root("repos"), State: read.State, Plan: plan, StateValue: stateRepos, PlanValue: planRepos, ConfigValue: planRepos}, modified)
	if modified.RequiresReplace {
		t.Fatal("matching configuration after import must not replace the environment")
	}
	updated := &resource.UpdateResponse{State: read.State}
	res.Update(ctx, resource.UpdateRequest{Plan: plan, State: read.State}, updated)
	if updated.Diagnostics.HasError() || len(rec.with(http.MethodPatch)) != 0 {
		t.Fatalf("update after import: %s patches %#v", fmtDiag(updated.Diagnostics), rec.with(http.MethodPatch))
	}

	idless, _ := lesClient(t, func(lesCall) (int, string) {
		return http.StatusOK, `{"owner":"team","name":"web","repos":[],"createdAt":"t","updatedAt":"t"}`
	})
	noID := &resource.ReadResponse{State: read.State}
	(&environmentResource{client: idless}).Read(ctx, resource.ReadRequest{State: read.State}, noID)
	if diags := noID.State.Get(ctx, &got); diags.HasError() || noID.Diagnostics.HasError() || got.ID.ValueString() != lesEnvID {
		t.Fatalf("a response without an id must not blank the id in state: %#v %s", got, fmtDiag(noID.Diagnostics))
	}

	status = http.StatusNotFound
	missing := &resource.ReadResponse{State: imported.State}
	res.Read(ctx, resource.ReadRequest{State: imported.State}, missing)
	if missing.Diagnostics.HasError() || !missing.State.Raw.IsNull() {
		t.Fatalf("importing a missing environment should leave no state: %s", fmtDiag(missing.Diagnostics))
	}
	del := &resource.DeleteResponse{State: read.State}
	res.Delete(ctx, resource.DeleteRequest{State: read.State}, del)
	if del.Diagnostics.HasError() {
		t.Fatalf("deleting a missing environment should succeed: %s", fmtDiag(del.Diagnostics))
	}
	status = http.StatusForbidden
	del = &resource.DeleteResponse{State: read.State}
	res.Delete(ctx, resource.DeleteRequest{State: read.State}, del)
	if !del.Diagnostics.HasError() {
		t.Fatal("a failed delete must be reported")
	}
}

const lesSecretID = "secv_3qKx9mT2bV7nR1cW5yZ8aQ"

func lesSecret(t *testing.T, repos ...string) secretModel {
	t.Helper()
	if repos == nil {
		repos = []string{}
	}
	list, diags := types.ListValueFrom(context.Background(), types.StringType, repos)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return secretModel{
		ID:              types.StringUnknown(),
		SecretVersionID: types.StringUnknown(),
		EnvironmentID:   types.StringValue(lesEnvID),
		Name:            types.StringValue("NPM_TOKEN"),
		Type:            types.StringValue("runtime_secret"),
		Repos:           list,
		ValueWO:         types.StringNull(),
		ValueWOVersion:  types.Int64Value(1),
	}
}

func lesTeam(model secretModel) teamSecretModel {
	return teamSecretModel{ID: model.ID, Name: model.Name, Type: model.Type, Repos: model.Repos, ValueWO: model.ValueWO, ValueWOVersion: model.ValueWOVersion, SecretVersionID: model.SecretVersionID}
}

func lesSecretList(t *testing.T, versions ...map[string]any) string {
	t.Helper()
	items := make([]any, 0, len(versions))
	for _, version := range versions {
		items = append(items, version)
	}
	return lesJSON(t, map[string]any{"items": items, "nextCursor": nil})
}

func lesVersion(id, name, kind string, repos ...string) map[string]any {
	if repos == nil {
		repos = []string{}
	}
	return map[string]any{"id": id, "name": name, "type": kind, "repos": repos, "createdAt": "t"}
}

func lesNoWait(t *testing.T) {
	t.Helper()
	saved := secretVersionPollDelays
	secretVersionPollDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { secretVersionPollDelays = saved })
}

// lesSecretCreate runs Create with value as the configured value_wo.
func lesSecretCreate(t *testing.T, res *secretResource, plan secretModel, value string) *resource.CreateResponse {
	t.Helper()
	ctx := context.Background()
	sch := resourceSchema(t, res)
	cfg := plan
	cfg.ID, cfg.SecretVersionID = types.StringNull(), types.StringNull()
	cfg.ValueWO = types.StringValue(value)
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	if res.scope == secretScopeTeam {
		teamPlan, teamCfg := lesTeam(plan), lesTeam(cfg)
		res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &teamPlan), Config: mustConfig(t, ctx, sch, &teamCfg)}, resp)
		return resp
	}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan), Config: mustConfig(t, ctx, sch, &cfg)}, resp)
	return resp
}

func TestSecretCreateRefusesExistingName(t *testing.T) {
	for name, scope := range map[string]secretScope{"environment": secretScopeEnvironment, "team": secretScopeTeam} {
		t.Run(name, func(t *testing.T) {
			client, rec := lesClient(t, func(call lesCall) (int, string) {
				if call.Method == http.MethodGet {
					return http.StatusOK, lesSecretList(t, lesVersion("secv_other", "OTHER", "runtime_secret"), lesVersion(lesSecretID, "NPM_TOKEN", "runtime_secret", "github.com/acme/api"))
				}
				return http.StatusOK, lesJSON(t, lesVersion(lesSecretID, "NPM_TOKEN", "runtime_secret"))
			})
			res := &secretResource{client: client, scope: scope}
			resp := lesSecretCreate(t, res, lesSecret(t), "super-secret-value")
			lpgWantPaths(t, resp.Diagnostics, "name")
			wantImport := lesEnvID + "/NPM_TOKEN/" + lesSecretID
			if scope == secretScopeTeam {
				wantImport = " NPM_TOKEN/" + lesSecretID
			}
			if detail := fmtDiag(resp.Diagnostics); !strings.Contains(detail, wantImport) || strings.Contains(detail, "super-secret-value") {
				t.Fatalf("diagnostics = %s", detail)
			}
			if puts := rec.with(http.MethodPut); len(puts) != 0 {
				t.Fatalf("an existing secret was overwritten: %#v", puts)
			}
			if !resp.State.Raw.IsNull() {
				t.Fatal("a refused create must not record state")
			}
		})
	}

	t.Run("list failure stops before the write", func(t *testing.T) {
		client, rec := lesClient(t, func(lesCall) (int, string) {
			return http.StatusForbidden, `{"error":{"code":"feature_unavailable","message":"no"}}`
		})
		resp := lesSecretCreate(t, &secretResource{client: client, scope: secretScopeTeam}, lesSecret(t), "v")
		if !strings.Contains(fmtDiag(resp.Diagnostics), "feature_unavailable") || len(rec.with(http.MethodPut)) != 0 {
			t.Fatalf("diagnostics = %s puts = %#v", fmtDiag(resp.Diagnostics), rec.with(http.MethodPut))
		}
	})
}

func TestSecretCreateKeepsPlannedValuesAndWarnsOnExistingVersion(t *testing.T) {
	ctx := context.Background()
	putStatus := http.StatusCreated
	client, rec := lesClient(t, func(call lesCall) (int, string) {
		if call.Method == http.MethodGet {
			return http.StatusOK, lesSecretList(t)
		}
		return putStatus, lesJSON(t, lesVersion(lesSecretID, "npm_token", "environment_variable", "github.com/acme/moved"))
	})
	res := &secretResource{client: client, scope: secretScopeEnvironment}
	plan := lesSecret(t, "https://github.com/Acme/api.git", "acme/web")
	resp := lesSecretCreate(t, res, plan, "super-secret-value")
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 0 {
		t.Fatalf("diagnostics = %s %s", fmtDiag(resp.Diagnostics), lesWarnings(resp.Diagnostics))
	}
	var got secretModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.ID.ValueString() != lesSecretID || got.SecretVersionID.ValueString() != lesSecretID || !got.Name.Equal(plan.Name) || !got.Type.Equal(plan.Type) || !got.Repos.Equal(plan.Repos) || !got.ValueWOVersion.Equal(plan.ValueWOVersion) || !got.EnvironmentID.Equal(plan.EnvironmentID) || !got.ValueWO.IsNull() {
		t.Fatalf("state = %#v, want the planned values with the API id", got)
	}
	if raw, _ := json.Marshal(resp.State.Raw); strings.Contains(string(raw), "super-secret-value") {
		t.Fatalf("state holds the secret value: %s", raw)
	}
	puts := rec.with(http.MethodPut)
	if len(puts) != 1 || puts[0].Target != "/v1/environments/"+lesEnvID+"/secrets/NPM_TOKEN" || !strings.Contains(puts[0].Body, `"value":"super-secret-value"`) || !strings.Contains(puts[0].Body, `"type":"runtime_secret"`) {
		t.Fatalf("puts = %#v", puts)
	}

	putStatus = http.StatusOK
	resp = lesSecretCreate(t, res, plan, "super-secret-value")
	if resp.Diagnostics.HasError() || !strings.Contains(lesWarnings(resp.Diagnostics), "existed before create") || strings.Contains(lesWarnings(resp.Diagnostics), "super-secret-value") {
		t.Fatalf("a 200 on create should warn: %s %s", fmtDiag(resp.Diagnostics), lesWarnings(resp.Diagnostics))
	}
}

func TestSecretCreateWaitsForVersionID(t *testing.T) {
	ctx := context.Background()
	lesNoWait(t)
	var mu sync.Mutex
	listsAfterPut, visibleAfter, written := 0, 2, false
	client, rec := lesClient(t, func(call lesCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		switch call.Method {
		case http.MethodPut:
			written = true
			return http.StatusCreated, `{"name":"NPM_TOKEN","type":"runtime_secret","repos":[]}`
		case http.MethodDelete:
			return http.StatusOK, `{"name":"NPM_TOKEN"}`
		}
		if !written {
			return http.StatusOK, lesSecretList(t)
		}
		listsAfterPut++
		if listsAfterPut <= visibleAfter {
			return http.StatusOK, lesSecretList(t, lesVersion("secv_other", "OTHER", "runtime_secret"))
		}
		return http.StatusOK, lesSecretList(t, lesVersion("secv_other", "OTHER", "runtime_secret"), lesVersion(lesSecretID, "NPM_TOKEN", "runtime_secret"))
	})
	res := &secretResource{client: client, scope: secretScopeTeam}
	lists := func() int {
		mu.Lock()
		defer mu.Unlock()
		return listsAfterPut
	}

	resp := lesSecretCreate(t, res, lesSecret(t), "v")
	if resp.Diagnostics.HasError() {
		t.Fatal(fmtDiag(resp.Diagnostics))
	}
	var got teamSecretModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.ID.ValueString() != lesSecretID || got.SecretVersionID.ValueString() != lesSecretID || lists() != 3 {
		t.Fatalf("state = %#v after %d lists", got, lists())
	}

	mu.Lock()
	listsAfterPut, visibleAfter, written = 0, 1000, false
	mu.Unlock()
	resp = lesSecretCreate(t, res, lesSecret(t), "v")
	if !resp.Diagnostics.HasError() || !strings.Contains(fmtDiag(resp.Diagnostics), "recorded in state") {
		t.Fatalf("diagnostics = %s", fmtDiag(resp.Diagnostics))
	}
	if lists() != len(secretVersionPollDelays)+1 {
		t.Fatalf("lists after the write = %d", lists())
	}
	if resp.State.Raw.IsNull() {
		t.Fatal("the created secret must be recorded so the next apply does not leave it unmanaged")
	}
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if !got.ID.IsNull() || !got.SecretVersionID.IsNull() || got.Name.ValueString() != "NPM_TOKEN" || got.Type.ValueString() != "runtime_secret" {
		t.Fatalf("partial state = %#v", got)
	}

	del := &resource.DeleteResponse{State: resp.State}
	res.Delete(ctx, resource.DeleteRequest{State: resp.State}, del)
	if deletes := rec.with(http.MethodDelete); del.Diagnostics.HasError() || len(deletes) != 1 || deletes[0].Target != "/v1/team/secrets/NPM_TOKEN" {
		t.Fatalf("delete without a version id: %s %#v", fmtDiag(del.Diagnostics), deletes)
	}

	mu.Lock()
	visibleAfter = 0
	mu.Unlock()
	read := &resource.ReadResponse{State: resp.State}
	res.Read(ctx, resource.ReadRequest{State: resp.State}, read)
	if read.Diagnostics.HasError() {
		t.Fatal(fmtDiag(read.Diagnostics))
	}
	if diags := read.State.Get(ctx, &got); diags.HasError() || got.SecretVersionID.ValueString() != lesSecretID || got.ID.ValueString() != lesSecretID {
		t.Fatalf("refresh should find the version by name: %#v %v", got, diags)
	}
}

func TestSecretCreateStopsWaitingWhenCancelled(t *testing.T) {
	saved := secretVersionPollDelays
	secretVersionPollDelays = []time.Duration{time.Hour}
	t.Cleanup(func() { secretVersionPollDelays = saved })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, _ := lesClient(t, func(call lesCall) (int, string) {
		if call.Method == http.MethodPut {
			return http.StatusCreated, `{"name":"NPM_TOKEN","type":"runtime_secret","repos":[]}`
		}
		return http.StatusOK, lesSecretList(t)
	})
	res := &secretResource{client: client, scope: secretScopeTeam}
	model := lesSecret(t)
	done := make(chan error, 1)
	go func() {
		_, err := res.resolveVersionID(ctx, client.public, model, nil)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait ignored cancellation")
	}
}

func TestSecretBlankRepositoryNeverBecomesEveryRepository(t *testing.T) {
	ctx := context.Background()
	client, rec := lesClient(t, func(call lesCall) (int, string) {
		if call.Method == http.MethodGet {
			return http.StatusOK, lesSecretList(t)
		}
		return http.StatusCreated, lesJSON(t, lesVersion(lesSecretID, "NPM_TOKEN", "runtime_secret"))
	})
	res := &secretResource{client: client, scope: secretScopeEnvironment}
	sch := resourceSchema(t, res)

	for _, repos := range [][]string{{""}, {"  "}, {"acme/api", ".git"}} {
		resp := lesSecretCreate(t, res, lesSecret(t, repos...), "v")
		if !resp.Diagnostics.HasError() || !strings.Contains(fmtDiag(resp.Diagnostics), "must not be blank") {
			t.Errorf("repos %q: %s", repos, fmtDiag(resp.Diagnostics))
		}
	}
	if puts := rec.with(http.MethodPut); len(puts) != 0 {
		t.Fatalf("a blank repository reached the API: %#v", puts)
	}
	for name, list := range map[string]types.List{"null": types.ListNull(types.StringType), "unknown": types.ListUnknown(types.StringType)} {
		if repos, diags := secretRepos(ctx, list); !diags.HasError() || repos != nil {
			t.Errorf("%s repos = %v, %s", name, repos, fmtDiag(diags))
		}
	}
	if repos, diags := secretRepos(ctx, lesSecret(t).Repos); diags.HasError() || repos == nil || len(repos) != 0 {
		t.Errorf("an explicitly empty list means every repository: %v %s", repos, fmtDiag(diags))
	}

	repoValidators := sch.Attributes["repos"].(schema.ListAttribute).Validators
	validate := func(list types.List) diag.Diagnostics {
		var out diag.Diagnostics
		for _, v := range repoValidators {
			resp := &validator.ListResponse{}
			v.ValidateList(ctx, validator.ListRequest{Path: path.Root("repos"), ConfigValue: list}, resp)
			out.Append(resp.Diagnostics...)
		}
		return out
	}
	lpgWantPaths(t, validate(lesSecret(t, "acme/api", " ", "").Repos), "repos[1]", "repos[2]")
	withUnknown := types.ListValueMust(types.StringType, []attr.Value{types.StringValue("acme/api"), types.StringUnknown()})
	for name, list := range map[string]types.List{"empty": lesSecret(t).Repos, "unknown element": withUnknown, "unknown list": types.ListUnknown(types.StringType)} {
		if diags := validate(list); diags.HasError() {
			t.Errorf("%s: %s", name, fmtDiag(diags))
		}
	}
	var many []string
	for i := range 101 {
		many = append(many, "acme/repo-"+strings.Repeat("x", i+1))
	}
	if diags := validate(lesSecret(t, many...).Repos); diags.ErrorsCount() != 1 {
		t.Errorf("101 repositories: %s", fmtDiag(diags))
	}
}

func TestSecretValueIsValidatedWithoutEchoingIt(t *testing.T) {
	ctx := context.Background()
	for _, res := range []*secretResource{{scope: secretScopeEnvironment}, {scope: secretScopeTeam}} {
		attribute := resourceSchema(t, res).Attributes["value_wo"].(schema.StringAttribute)
		if !attribute.Sensitive || !attribute.WriteOnly || !attribute.Required {
			t.Fatalf("value_wo must be required, sensitive and write-only: %#v", attribute)
		}
		check := func(value types.String) diag.Diagnostics {
			var out diag.Diagnostics
			for _, v := range attribute.Validators {
				resp := &validator.StringResponse{}
				v.ValidateString(ctx, validator.StringRequest{Path: path.Root("value_wo"), ConfigValue: value}, resp)
				out.Append(resp.Diagnostics...)
			}
			return out
		}
		tooLong := strings.Repeat("s3cr3t", 700)
		for name, tc := range map[string]struct {
			value types.String
			valid bool
		}{
			"one byte":   {types.StringValue("x"), true},
			"4096 bytes": {types.StringValue(strings.Repeat("x", 4096)), true},
			"4097 bytes": {types.StringValue(strings.Repeat("x", 4097)), false},
			"multibyte":  {types.StringValue(strings.Repeat("é", 2049)), false},
			"empty":      {types.StringValue(""), false},
			"too long":   {types.StringValue(tooLong), false},
			"unknown":    {types.StringUnknown(), true},
			"null":       {types.StringNull(), true},
		} {
			diags := check(tc.value)
			if tc.valid == diags.HasError() {
				t.Errorf("%s: %s", name, fmtDiag(diags))
			}
			if detail := fmtDiag(diags); strings.Contains(detail, "s3cr3t") || strings.Contains(detail, "4200") {
				t.Errorf("%s: the diagnostic echoes the value or its length: %s", name, detail)
			}
		}
	}
}

func TestSecretReadRemovesOnlyWhatIsGone(t *testing.T) {
	ctx := context.Background()
	status, body := http.StatusOK, ""
	client, _ := lesClient(t, func(lesCall) (int, string) { return status, body })
	read := func(scope secretScope) *resource.ReadResponse {
		t.Helper()
		res := &secretResource{client: client, scope: scope}
		sch := resourceSchema(t, res)
		prior := lesSecret(t, "acme/api")
		prior.ID, prior.SecretVersionID = types.StringValue(lesSecretID), types.StringValue(lesSecretID)
		var state tfsdk.State
		if scope == secretScopeTeam {
			team := lesTeam(prior)
			state = mustState(t, ctx, sch, &team)
		} else {
			state = mustState(t, ctx, sch, &prior)
		}
		resp := &resource.ReadResponse{State: state}
		res.Read(ctx, resource.ReadRequest{State: state}, resp)
		return resp
	}

	status, body = http.StatusNotFound, `{"error":{"code":"environment_not_found","message":"missing"}}`
	if resp := read(secretScopeEnvironment); resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
		t.Errorf("a missing environment removes its secret from state: %s", fmtDiag(resp.Diagnostics))
	}
	if resp := read(secretScopeTeam); !resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		t.Errorf("a 404 from the team list is a failed read, not a deleted secret: %s", fmtDiag(resp.Diagnostics))
	}

	status, body = http.StatusOK, lesSecretList(t, lesVersion("secv_other", "NPM_TOKEN", "runtime_secret"))
	for _, scope := range []secretScope{secretScopeEnvironment, secretScopeTeam} {
		if resp := read(scope); resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
			t.Errorf("a version that is no longer listed is removed from state: %s", fmtDiag(resp.Diagnostics))
		}
	}

	status, body = http.StatusInternalServerError, `{"error":{"code":"internal","message":"boom"}}`
	for _, scope := range []secretScope{secretScopeEnvironment, secretScopeTeam} {
		if resp := read(scope); !resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
			t.Errorf("a failed list must not remove the secret: %s", fmtDiag(resp.Diagnostics))
		}
	}

	status, body = http.StatusOK, lesSecretList(t, lesVersion(lesSecretID, "NPM_TOKEN", "environment_variable", "github.com/acme/api", "github.com/acme/web"))
	resp := read(secretScopeEnvironment)
	var got secretModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() || resp.Diagnostics.HasError() {
		t.Fatal(diags, resp.Diagnostics)
	}
	if got.Type.ValueString() != "environment_variable" || len(got.Repos.Elements()) != 2 || got.ValueWOVersion.ValueInt64() != 1 || !got.ValueWO.IsNull() {
		t.Fatalf("refresh should show type and repos drift and keep value_wo_version: %#v", got)
	}
}

func TestSecretChangedToBuildSecretCanBeDestroyedButNotUpdated(t *testing.T) {
	ctx := context.Background()
	client, rec := lesClient(t, func(call lesCall) (int, string) {
		if call.Method == http.MethodDelete {
			return http.StatusOK, `{"name":"NPM_TOKEN"}`
		}
		return http.StatusOK, lesSecretList(t, lesVersion(lesSecretID, "NPM_TOKEN", "build_secret"))
	})
	res := &secretResource{client: client, scope: secretScopeEnvironment}
	sch := resourceSchema(t, res)
	prior := lesSecret(t)
	prior.ID, prior.SecretVersionID = types.StringValue(lesSecretID), types.StringValue(lesSecretID)
	state := mustState(t, ctx, sch, &prior)

	read := &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, read)
	if read.Diagnostics.HasError() || !strings.Contains(lesWarnings(read.Diagnostics), "Build Secret") {
		t.Fatalf("refresh should warn and keep the resource readable: %s %s", fmtDiag(read.Diagnostics), lesWarnings(read.Diagnostics))
	}
	var got secretModel
	if diags := read.State.Get(ctx, &got); diags.HasError() || got.Type.ValueString() != "build_secret" {
		t.Fatalf("state = %#v %v", got, diags)
	}

	modify := func(res *secretResource, plan tfsdk.Plan, state tfsdk.State) diag.Diagnostics {
		resp := &resource.ModifyPlanResponse{Plan: plan}
		res.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state, Config: tfsdk.Config{Schema: sch, Raw: plan.Raw}}, resp)
		return resp.Diagnostics
	}
	lpgWantPaths(t, modify(res, mustPlan(t, ctx, sch, &prior), read.State), "type")
	lpgWantPaths(t, modify(&secretResource{scope: secretScopeEnvironment}, mustPlan(t, ctx, sch, &prior), read.State), "type")

	renamed := prior
	renamed.Name = types.StringValue("NPM_TOKEN_2")
	moved := prior
	moved.EnvironmentID = types.StringUnknown()
	for name, tc := range map[string]struct {
		plan  tfsdk.Plan
		state tfsdk.State
	}{
		"destroy":                 {tfsdk.Plan{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}, read.State},
		"create":                  {mustPlan(t, ctx, sch, &prior), emptyState(ctx, sch)},
		"replace by name":         {mustPlan(t, ctx, sch, &renamed), read.State},
		"replace by environment":  {mustPlan(t, ctx, sch, &moved), read.State},
		"ordinary secret version": {mustPlan(t, ctx, sch, &prior), state},
	} {
		if diags := modify(res, tc.plan, tc.state); diags.HasError() {
			t.Errorf("%s: %s", name, fmtDiag(diags))
		}
	}

	del := &resource.DeleteResponse{State: read.State}
	res.Delete(ctx, resource.DeleteRequest{State: read.State}, del)
	if deletes := rec.with(http.MethodDelete); del.Diagnostics.HasError() || len(deletes) != 1 || !strings.HasSuffix(deletes[0].Target, "/secrets/NPM_TOKEN?id="+lesSecretID) {
		t.Fatalf("delete: %s %#v", fmtDiag(del.Diagnostics), deletes)
	}

	imported := &resource.ImportStateResponse{State: emptyState(ctx, sch)}
	res.ImportState(ctx, resource.ImportStateRequest{ID: lesEnvID + "/NPM_TOKEN/" + lesSecretID}, imported)
	importRead := &resource.ReadResponse{State: imported.State}
	res.Read(ctx, resource.ReadRequest{State: imported.State}, importRead)
	if !strings.Contains(fmtDiag(importRead.Diagnostics), "cannot be imported") {
		t.Fatalf("importing a Build Secret should fail: %s", fmtDiag(importRead.Diagnostics))
	}
}

func TestSecretImport(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		scope            secretScope
		id               string
		env, name, secID string
		valid            bool
	}{
		{secretScopeEnvironment, lesEnvID + "/NPM_TOKEN/" + lesSecretID, lesEnvID, "NPM_TOKEN", lesSecretID, true},
		{secretScopeEnvironment, " " + lesEnvID + "/NPM_TOKEN ", lesEnvID, "NPM_TOKEN", "", true},
		{secretScopeEnvironment, "NPM_TOKEN", "", "", "", false},
		{secretScopeEnvironment, "/NPM_TOKEN", "", "", "", false},
		{secretScopeEnvironment, lesEnvID + "/", "", "", "", false},
		{secretScopeEnvironment, lesEnvID + "//" + lesSecretID, "", "", "", false},
		{secretScopeEnvironment, "", "", "", "", false},
		{secretScopeTeam, "NPM_TOKEN/" + lesSecretID, "", "NPM_TOKEN", lesSecretID, true},
		{secretScopeTeam, "NPM_TOKEN", "", "NPM_TOKEN", "", true},
		{secretScopeTeam, "NPM_TOKEN/", "", "", "", false},
		{secretScopeTeam, "/" + lesSecretID, "", "", "", false},
		{secretScopeTeam, "  ", "", "", "", false},
	} {
		env, name, secID, err := parseSecretImport(tc.scope, tc.id)
		if tc.valid != (err == nil) || env != tc.env || name != tc.name || secID != tc.secID {
			t.Errorf("parseSecretImport(%q) = %q %q %q %v", tc.id, env, name, secID, err)
		}
		if err != nil && !strings.Contains(err.Error(), "<name>") {
			t.Errorf("parseSecretImport(%q) error does not show the format: %v", tc.id, err)
		}
	}

	body := lesSecretList(t, lesVersion(lesSecretID, "NPM_TOKEN", "environment_variable", "github.com/acme/api"), lesVersion("secv_other", "OTHER", "runtime_secret"))
	client, rec := lesClient(t, func(lesCall) (int, string) { return http.StatusOK, body })
	res := &secretResource{client: client, scope: secretScopeEnvironment}
	sch := resourceSchema(t, res)
	importRead := func(id string) *resource.ReadResponse {
		t.Helper()
		imported := &resource.ImportStateResponse{State: emptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: id}, imported)
		if imported.Diagnostics.HasError() {
			t.Fatal(fmtDiag(imported.Diagnostics))
		}
		read := &resource.ReadResponse{State: imported.State}
		res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
		return read
	}

	for _, id := range []string{lesEnvID + "/NPM_TOKEN/" + lesSecretID, lesEnvID + "/NPM_TOKEN"} {
		read := importRead(id)
		if read.Diagnostics.HasError() {
			t.Fatal(fmtDiag(read.Diagnostics))
		}
		var got secretModel
		if diags := read.State.Get(ctx, &got); diags.HasError() {
			t.Fatal(diags)
		}
		wantRepos, _ := types.ListValueFrom(ctx, types.StringType, []string{"github.com/acme/api"})
		if got.ID.ValueString() != lesSecretID || got.SecretVersionID.ValueString() != lesSecretID || got.EnvironmentID.ValueString() != lesEnvID || got.Name.ValueString() != "NPM_TOKEN" || got.Type.ValueString() != "environment_variable" || !got.Repos.Equal(wantRepos) || !got.ValueWO.IsNull() || !got.ValueWOVersion.IsNull() {
			t.Fatalf("import %q state = %#v", id, got)
		}
	}
	if gets := rec.with(http.MethodGet); len(gets) != 2 || gets[0].Target != "/v1/environments/"+lesEnvID+"/secrets" {
		t.Fatalf("list calls = %#v", gets)
	}

	if read := importRead(lesEnvID + "/NPM_TOKEN/secv_other"); !strings.Contains(fmtDiag(read.Diagnostics), "belongs to the secret named OTHER") {
		t.Errorf("a version id of another name should be refused: %s", fmtDiag(read.Diagnostics))
	}
	for _, id := range []string{lesEnvID + "/NPM_TOKEN/secv_missing", lesEnvID + "/MISSING", lesEnvID + "/npm_token"} {
		if read := importRead(id); read.Diagnostics.HasError() || !read.State.Raw.IsNull() {
			t.Errorf("import %q of a missing secret should leave no state: %s", id, fmtDiag(read.Diagnostics))
		}
	}

	// Outside import, a version whose name changed is drift that plans a replacement.
	prior := lesSecret(t, "acme/api")
	prior.ID, prior.SecretVersionID = types.StringValue("secv_other"), types.StringValue("secv_other")
	state := mustState(t, ctx, sch, &prior)
	read := &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, read)
	var got secretModel
	if diags := read.State.Get(ctx, &got); diags.HasError() || read.Diagnostics.HasError() || got.Name.ValueString() != "OTHER" {
		t.Fatalf("renamed version: %#v %s", got, fmtDiag(read.Diagnostics))
	}
}

func TestSecretUpdate(t *testing.T) {
	ctx := context.Background()
	putStatus, putBody := http.StatusOK, `{"name":"NPM_TOKEN","type":"runtime_secret","repos":["github.com/acme/api"]}`
	client, rec := lesClient(t, func(call lesCall) (int, string) {
		if call.Method == http.MethodPut {
			return putStatus, putBody
		}
		return http.StatusInternalServerError, `{"error":{"code":"x","message":"update should not list"}}`
	})
	res := &secretResource{client: client, scope: secretScopeEnvironment}
	sch := resourceSchema(t, res)
	prior := lesSecret(t, "acme/api")
	prior.ID, prior.SecretVersionID = types.StringValue(lesSecretID), types.StringValue(lesSecretID)
	priorState := mustState(t, ctx, sch, &prior)
	update := func(plan secretModel) *resource.UpdateResponse {
		t.Helper()
		cfg := plan
		cfg.ID, cfg.SecretVersionID = types.StringNull(), types.StringNull()
		cfg.ValueWO = types.StringValue("rotated-secret-value")
		resp := &resource.UpdateResponse{State: priorState}
		res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: priorState, Config: mustConfig(t, ctx, sch, &cfg)}, resp)
		return resp
	}

	plan := prior
	plan.ValueWOVersion = types.Int64Value(2)
	plan.Type = types.StringValue("environment_variable")
	plan.Repos, _ = types.ListValueFrom(ctx, types.StringType, []string{"https://github.com/Acme/web.git", "acme/api"})
	resp := update(plan)
	if resp.Diagnostics.HasError() {
		t.Fatal(fmtDiag(resp.Diagnostics))
	}
	puts := rec.with(http.MethodPut)
	if len(puts) != 1 || puts[0].Target != "/v1/environments/"+lesEnvID+"/secrets/NPM_TOKEN?id="+lesSecretID || puts[0].Body != `{"value":"rotated-secret-value","type":"environment_variable","repos":["github.com/acme/api","github.com/acme/web"]}` {
		t.Fatalf("puts = %#v", puts)
	}
	if gets := rec.with(http.MethodGet); len(gets) != 0 {
		t.Fatalf("update listed secrets: %#v", gets)
	}
	var got secretModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.ID.ValueString() != lesSecretID || got.SecretVersionID.ValueString() != lesSecretID || !got.Type.Equal(plan.Type) || !got.Repos.Equal(plan.Repos) || got.ValueWOVersion.ValueInt64() != 2 || !got.ValueWO.IsNull() {
		t.Fatalf("state = %#v, want the planned values and the same version id", got)
	}
	if raw, _ := json.Marshal(resp.State.Raw); strings.Contains(string(raw), "rotated-secret-value") {
		t.Fatalf("state holds the secret value: %s", raw)
	}

	putStatus, putBody = http.StatusNotFound, `{"error":{"code":"secret_not_found","message":"missing"}}`
	resp = update(plan)
	if !resp.Diagnostics.HasError() || !strings.Contains(fmtDiag(resp.Diagnostics), "no longer exists") || strings.Contains(fmtDiag(resp.Diagnostics), "rotated-secret-value") {
		t.Fatalf("diagnostics = %s", fmtDiag(resp.Diagnostics))
	}
	if diags := resp.State.Get(ctx, &got); diags.HasError() || got.ValueWOVersion.ValueInt64() != 1 || got.Type.ValueString() != "runtime_secret" {
		t.Fatalf("a failed update must keep the prior state: %#v", got)
	}

	missingValue := &resource.UpdateResponse{State: priorState}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: priorState, Config: mustConfig(t, ctx, sch, &plan)}, missingValue)
	lpgWantPaths(t, missingValue.Diagnostics, "value_wo")
}

func TestSecretDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	status, body := http.StatusNotFound, `{"error":{"code":"secret_not_found","message":"missing"}}`
	client, rec := lesClient(t, func(lesCall) (int, string) { return status, body })
	res := &secretResource{client: client, scope: secretScopeTeam}
	sch := resourceSchema(t, res)
	prior := lesTeam(lesSecret(t))
	prior.ID, prior.SecretVersionID = types.StringValue(lesSecretID), types.StringValue(lesSecretID)
	state := mustState(t, ctx, sch, &prior)

	resp := &resource.DeleteResponse{State: state}
	res.Delete(ctx, resource.DeleteRequest{State: state}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("deleting a missing secret should succeed: %s", fmtDiag(resp.Diagnostics))
	}
	if deletes := rec.with(http.MethodDelete); len(deletes) != 1 || deletes[0].Target != "/v1/team/secrets/NPM_TOKEN?id="+lesSecretID {
		t.Fatalf("deletes = %#v", deletes)
	}

	status, body = http.StatusForbidden, `{"error":{"code":"team_admin_required","message":"admins only"}}`
	resp = &resource.DeleteResponse{State: state}
	res.Delete(ctx, resource.DeleteRequest{State: state}, resp)
	if !strings.Contains(fmtDiag(resp.Diagnostics), "team_admin_required") {
		t.Fatalf("a refused delete must be reported: %s", fmtDiag(resp.Diagnostics))
	}
}

// lesProvider drives a resource through the provider server the way Terraform does, so the framework's own handling of plans, write-only values and replacement is part of what is tested.
type lesProvider struct {
	t       *testing.T
	ctx     context.Context
	server  tfprotov6.ProviderServer
	schemas *tfprotov6.GetProviderSchemaResponse
}

func newLesProvider(t *testing.T, client *apiClient) *lesProvider {
	t.Helper()
	ctx := context.Background()
	server, err := providerserver.NewProtocol6WithError(mockClientProvider{Provider: New("test")(), client: client})()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	p := &lesProvider{t: t, ctx: ctx, server: server, schemas: schemas}
	providerType := schemas.Provider.ValueType()
	nulls := map[string]tftypes.Value{}
	for name, typ := range providerType.(tftypes.Object).AttributeTypes {
		nulls[name] = tftypes.NewValue(typ, nil)
	}
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: p.encode(providerType, tftypes.NewValue(providerType, nulls))})
	if err != nil {
		t.Fatal(err)
	}
	p.noErrors("configure", configured.Diagnostics)
	return p
}

func (p *lesProvider) typ(typeName string) tftypes.Object {
	p.t.Helper()
	sch, ok := p.schemas.ResourceSchemas[typeName]
	if !ok {
		p.t.Fatalf("provider has no resource %s", typeName)
	}
	return sch.ValueType().(tftypes.Object)
}

func (p *lesProvider) encode(typ tftypes.Type, value tftypes.Value) *tfprotov6.DynamicValue {
	p.t.Helper()
	encoded, err := tfprotov6.NewDynamicValue(typ, value)
	if err != nil {
		p.t.Fatal(err)
	}
	return &encoded
}

func (p *lesProvider) decode(typeName string, value *tfprotov6.DynamicValue) tftypes.Value {
	p.t.Helper()
	decoded, err := value.Unmarshal(p.typ(typeName))
	if err != nil {
		p.t.Fatal(err)
	}
	return decoded
}

func (p *lesProvider) errors(diags []*tfprotov6.Diagnostic) string {
	var out []string
	for _, d := range diags {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			out = append(out, d.Summary+": "+d.Detail)
		}
	}
	return strings.Join(out, "; ")
}

func (p *lesProvider) noErrors(step string, diags []*tfprotov6.Diagnostic) {
	p.t.Helper()
	if text := p.errors(diags); text != "" {
		p.t.Fatalf("%s: %s", step, text)
	}
}

func (p *lesProvider) attrs(value tftypes.Value) map[string]tftypes.Value {
	p.t.Helper()
	out := map[string]tftypes.Value{}
	if err := value.As(&out); err != nil {
		p.t.Fatal(err)
	}
	copied := make(map[string]tftypes.Value, len(out))
	for name, v := range out {
		copied[name] = v
	}
	return copied
}

func (p *lesProvider) text(value tftypes.Value, name string) string {
	p.t.Helper()
	var out string
	if err := p.attrs(value)[name].As(&out); err != nil {
		p.t.Fatalf("%s: %v", name, err)
	}
	return out
}

// config builds a configuration object: every attribute null except the ones given.
func (p *lesProvider) config(typeName string, set map[string]tftypes.Value) tftypes.Value {
	typ := p.typ(typeName)
	values := map[string]tftypes.Value{}
	for name, attrType := range typ.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	for name, value := range set {
		values[name] = value
	}
	return tftypes.NewValue(typ, values)
}

func (p *lesProvider) validate(typeName string, config tftypes.Value) string {
	p.t.Helper()
	resp, err := p.server.ValidateResourceConfig(p.ctx, &tfprotov6.ValidateResourceConfigRequest{
		TypeName:           typeName,
		Config:             p.encode(p.typ(typeName), config),
		ClientCapabilities: &tfprotov6.ValidateResourceConfigClientCapabilities{WriteOnlyAttributesAllowed: true},
	})
	if err != nil {
		p.t.Fatal(err)
	}
	return p.errors(resp.Diagnostics)
}

// plan proposes what Terraform proposes: the configuration, with computed attributes the configuration leaves null carried over from prior state.
func (p *lesProvider) plan(typeName string, prior, config tftypes.Value) (tftypes.Value, []string, *tfprotov6.PlanResourceChangeResponse) {
	p.t.Helper()
	typ := p.typ(typeName)
	proposed := config
	if !prior.IsNull() {
		priorAttrs, values := p.attrs(prior), p.attrs(config)
		for _, attribute := range p.schemas.ResourceSchemas[typeName].Block.Attributes {
			if attribute.Computed && values[attribute.Name].IsNull() {
				values[attribute.Name] = priorAttrs[attribute.Name]
			}
		}
		proposed = tftypes.NewValue(typ, values)
	}
	resp, err := p.server.PlanResourceChange(p.ctx, &tfprotov6.PlanResourceChangeRequest{TypeName: typeName, PriorState: p.encode(typ, prior), ProposedNewState: p.encode(typ, proposed), Config: p.encode(typ, config)})
	if err != nil {
		p.t.Fatal(err)
	}
	p.noErrors(typeName+" plan", resp.Diagnostics)
	var replace []string
	for _, attributePath := range resp.RequiresReplace {
		replace = append(replace, attributePath.String())
	}
	return p.decode(typeName, resp.PlannedState), replace, resp
}

// apply plans and applies config in place and holds the result to the plan the way Terraform does: every value the plan knew must be the value in the new state, and nothing may stay unknown.
func (p *lesProvider) apply(typeName string, prior, config tftypes.Value) tftypes.Value {
	p.t.Helper()
	planned, replace, plan := p.plan(typeName, prior, config)
	if len(replace) != 0 {
		p.t.Fatalf("%s: plan wants to replace %v", typeName, replace)
	}
	typ := p.typ(typeName)
	resp, err := p.server.ApplyResourceChange(p.ctx, &tfprotov6.ApplyResourceChangeRequest{TypeName: typeName, PriorState: p.encode(typ, prior), PlannedState: plan.PlannedState, PlannedPrivate: plan.PlannedPrivate, Config: p.encode(typ, config)})
	if err != nil {
		p.t.Fatal(err)
	}
	p.noErrors(typeName+" apply", resp.Diagnostics)
	state := p.decode(typeName, resp.NewState)
	if !state.IsFullyKnown() {
		p.t.Fatalf("%s: state after apply has unknown values: %s", typeName, state)
	}
	applied := p.attrs(state)
	for name, want := range p.attrs(planned) {
		if want.IsFullyKnown() && !want.Equal(applied[name]) {
			p.t.Fatalf("%s.%s: planned %s, applied %s", typeName, name, want, applied[name])
		}
	}
	return state
}

func (p *lesProvider) noChanges(step, typeName string, state, config tftypes.Value) {
	p.t.Helper()
	planned, replace, _ := p.plan(typeName, state, config)
	if !planned.Equal(state) || len(replace) != 0 {
		p.t.Fatalf("%s: plan is not empty: state %s, planned %s, replace %v", step, state, planned, replace)
	}
}

func (p *lesProvider) read(typeName string, state tftypes.Value) tftypes.Value {
	p.t.Helper()
	resp, err := p.server.ReadResource(p.ctx, &tfprotov6.ReadResourceRequest{TypeName: typeName, CurrentState: p.encode(p.typ(typeName), state)})
	if err != nil {
		p.t.Fatal(err)
	}
	p.noErrors(typeName+" read", resp.Diagnostics)
	return p.decode(typeName, resp.NewState)
}

func (p *lesProvider) importState(typeName, id string) tftypes.Value {
	p.t.Helper()
	resp, err := p.server.ImportResourceState(p.ctx, &tfprotov6.ImportResourceStateRequest{TypeName: typeName, ID: id})
	if err != nil {
		p.t.Fatal(err)
	}
	p.noErrors(typeName+" import", resp.Diagnostics)
	if len(resp.ImportedResources) != 1 {
		p.t.Fatalf("%s import returned %d resources", typeName, len(resp.ImportedResources))
	}
	return p.decode(typeName, resp.ImportedResources[0].State)
}

func (p *lesProvider) destroy(typeName string, prior tftypes.Value) {
	p.t.Helper()
	typ := p.typ(typeName)
	null := p.encode(typ, tftypes.NewValue(typ, nil))
	resp, err := p.server.ApplyResourceChange(p.ctx, &tfprotov6.ApplyResourceChangeRequest{TypeName: typeName, PriorState: p.encode(typ, prior), PlannedState: null, Config: null})
	if err != nil {
		p.t.Fatal(err)
	}
	p.noErrors(typeName+" destroy", resp.Diagnostics)
	if state := p.decode(typeName, resp.NewState); !state.IsNull() {
		p.t.Fatalf("%s: state after destroy = %s", typeName, state)
	}
}

func lesString(value string) tftypes.Value { return tftypes.NewValue(tftypes.String, value) }

func TestEnvironmentLifecycleThroughTheProviderServer(t *testing.T) {
	const typeName = "cursor_environment"
	var mu sync.Mutex
	var stored map[string]any
	client, rec := lesClient(t, func(call lesCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		missing := `{"error":{"code":"environment_not_found","message":"missing"}}`
		switch {
		case call.Method == http.MethodPost:
			var req struct {
				Owner, Name, EnvironmentJSON string
				Repos                        []struct{ URL string }
			}
			if err := json.Unmarshal([]byte(call.Body), &req); err != nil {
				return http.StatusBadRequest, `{}`
			}
			// The service is free to store its own spelling of a repository and of the JSON document.
			urls := make([]string, 0, len(req.Repos))
			for _, repo := range req.Repos {
				urls = append(urls, "https://"+publicapi.CanonicalRepo(repo.URL))
			}
			stored = lesEnvironmentBody(t, req.Name, strings.ReplaceAll(req.EnvironmentJSON, " ", ""), urls...)
			return http.StatusCreated, lesJSON(t, stored)
		case stored == nil:
			return http.StatusNotFound, missing
		case call.Method == http.MethodGet:
			return http.StatusOK, lesJSON(t, stored)
		case call.Method == http.MethodPatch:
			var req map[string]string
			if err := json.Unmarshal([]byte(call.Body), &req); err != nil {
				return http.StatusBadRequest, `{}`
			}
			if name, ok := req["name"]; ok {
				stored["name"] = name
			}
			if doc, ok := req["environmentJson"]; ok {
				stored["environmentJson"] = doc
			}
			return http.StatusNoContent, ""
		case call.Method == http.MethodDelete:
			stored = nil
			return http.StatusOK, `{"id":"` + lesEnvID + `"}`
		}
		return http.StatusNotFound, missing
	})
	p := newLesProvider(t, client)
	reposType := p.typ(typeName).AttributeTypes["repos"].(tftypes.List)
	config := func(name, doc string, urls ...string) tftypes.Value {
		repos := make([]tftypes.Value, 0, len(urls))
		for _, url := range urls {
			repos = append(repos, tftypes.NewValue(reposType.ElementType, map[string]tftypes.Value{"url": lesString(url)}))
		}
		return p.config(typeName, map[string]tftypes.Value{
			"owner":            lesString("team"),
			"name":             lesString(name),
			"environment_json": lesString(doc),
			"repos":            tftypes.NewValue(reposType, repos),
		})
	}

	first := config("web", `{"install": "pnpm"}`, "git@github.com:Acme/api.git")
	if errs := p.validate(typeName, first); errs != "" {
		t.Fatalf("validate: %s", errs)
	}
	state := p.apply(typeName, tftypes.NewValue(p.typ(typeName), nil), first)
	if got := p.text(state, "id"); got != lesEnvID {
		t.Fatalf("id = %q", got)
	}
	if refreshed := p.read(typeName, state); !refreshed.Equal(state) {
		t.Fatalf("refresh drifted: %s -> %s", state, refreshed)
	}
	p.noChanges("after create", typeName, state, first)

	renamed := config("web-2", `{"install": "pnpm"}`, "git@github.com:Acme/api.git")
	state = p.apply(typeName, state, renamed)
	if patches := rec.with(http.MethodPatch); len(patches) != 1 || patches[0].Body != `{"name":"web-2"}` {
		t.Fatalf("a rename should send only the name: %#v", patches)
	}
	p.noChanges("after rename", typeName, p.read(typeName, state), renamed)

	respelled := config("web-2", `{"install":"pnpm"}`, "https://github.com/acme/api")
	state = p.apply(typeName, state, respelled)
	if patches := rec.with(http.MethodPatch); len(patches) != 1 {
		t.Fatalf("a different spelling of the same settings should send nothing: %#v", patches)
	}
	p.noChanges("after respelling", typeName, p.read(typeName, state), respelled)

	if _, replace, _ := p.plan(typeName, state, config("web-2", `{"install":"pnpm"}`, "https://github.com/acme/other")); len(replace) != 1 || !strings.Contains(replace[0], "repos") {
		t.Fatalf("a different repository should replace the environment: %v", replace)
	}

	imported := p.read(typeName, p.importState(typeName, lesEnvID))
	if got := p.apply(typeName, imported, respelled); !got.Equal(state) {
		t.Fatalf("state after import and apply = %s, want %s", got, state)
	}
	if patches := rec.with(http.MethodPatch); len(patches) != 1 {
		t.Fatalf("adopting an imported environment should send nothing: %#v", patches)
	}

	p.destroy(typeName, state)
	if deletes := rec.with(http.MethodDelete); len(deletes) != 1 || deletes[0].Target != "/v1/environments/"+lesEnvID {
		t.Fatalf("deletes = %#v", deletes)
	}
	if gone := p.read(typeName, state); !gone.IsNull() {
		t.Fatalf("an environment deleted outside Terraform should leave state: %s", gone)
	}
}

func TestTeamSecretLifecycleThroughTheProviderServer(t *testing.T) {
	const typeName = "cursor_team_secret"
	const firstValue, secondValue = "first-secret-value", "second-secret-value"
	var mu sync.Mutex
	var stored map[string]any
	client, rec := lesClient(t, func(call lesCall) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		switch call.Method {
		case http.MethodGet:
			if stored == nil {
				return http.StatusOK, lesSecretList(t)
			}
			return http.StatusOK, lesSecretList(t, stored)
		case http.MethodPut:
			var req struct {
				Value, Type string
				Repos       []string
			}
			if err := json.Unmarshal([]byte(call.Body), &req); err != nil {
				return http.StatusBadRequest, `{}`
			}
			status := http.StatusOK
			if stored == nil {
				status = http.StatusCreated
			}
			stored = lesVersion(lesSecretID, "NPM_TOKEN", req.Type, publicapi.CanonicalRepos(req.Repos)...)
			return status, lesJSON(t, stored)
		case http.MethodDelete:
			stored = nil
			return http.StatusOK, `{"name":"NPM_TOKEN"}`
		}
		return http.StatusNotFound, `{}`
	})
	p := newLesProvider(t, client)
	reposType := p.typ(typeName).AttributeTypes["repos"]
	config := func(name, value string, version int, repos ...string) tftypes.Value {
		elements := make([]tftypes.Value, 0, len(repos))
		for _, repo := range repos {
			elements = append(elements, lesString(repo))
		}
		return p.config(typeName, map[string]tftypes.Value{
			"name":             lesString(name),
			"type":             lesString("runtime_secret"),
			"repos":            tftypes.NewValue(reposType, elements),
			"value_wo":         lesString(value),
			"value_wo_version": tftypes.NewValue(tftypes.Number, version),
		})
	}
	// Terraform core holds write-only values back from plans and state, so what it compares is the configuration without them.
	withoutValue := func(config tftypes.Value) tftypes.Value {
		values := p.attrs(config)
		values["value_wo"] = tftypes.NewValue(tftypes.String, nil)
		return tftypes.NewValue(p.typ(typeName), values)
	}
	noSecrets := func(step string, state tftypes.Value) {
		t.Helper()
		if text := state.String(); strings.Contains(text, firstValue) || strings.Contains(text, secondValue) {
			t.Fatalf("%s: state holds the secret value: %s", step, text)
		}
	}

	first := config("NPM_TOKEN", firstValue, 1, "git@github.com:Acme/api.git")
	if errs := p.validate(typeName, first); errs != "" {
		t.Fatalf("validate: %s", errs)
	}
	state := p.apply(typeName, tftypes.NewValue(p.typ(typeName), nil), first)
	noSecrets("create", state)
	if id, version := p.text(state, "id"), p.text(state, "secret_version_id"); id != lesSecretID || version != lesSecretID {
		t.Fatalf("id = %q, secret_version_id = %q", id, version)
	}
	if puts := rec.with(http.MethodPut); len(puts) != 1 || puts[0].Target != "/v1/team/secrets/NPM_TOKEN" || !strings.Contains(puts[0].Body, firstValue) {
		t.Fatalf("create puts = %#v", puts)
	}
	if refreshed := p.read(typeName, state); !refreshed.Equal(state) {
		t.Fatalf("refresh drifted: %s -> %s", state, refreshed)
	}
	p.noChanges("after create", typeName, state, withoutValue(first))

	rotated := config("NPM_TOKEN", secondValue, 2, "git@github.com:Acme/api.git")
	state = p.apply(typeName, state, rotated)
	noSecrets("rotate", state)
	if puts := rec.with(http.MethodPut); len(puts) != 2 || puts[1].Target != "/v1/team/secrets/NPM_TOKEN?id="+lesSecretID || !strings.Contains(puts[1].Body, secondValue) {
		t.Fatalf("a rotation should change the same version in place: %#v", puts)
	}
	p.noChanges("after rotate", typeName, p.read(typeName, state), withoutValue(rotated))

	if _, replace, _ := p.plan(typeName, state, config("OTHER_TOKEN", secondValue, 2, "git@github.com:Acme/api.git")); len(replace) != 1 || !strings.Contains(replace[0], "name") {
		t.Fatalf("a different name should replace the secret: %v", replace)
	}

	imported := p.read(typeName, p.importState(typeName, "NPM_TOKEN"))
	noSecrets("import", imported)
	if got := p.text(imported, "secret_version_id"); got != lesSecretID {
		t.Fatalf("imported secret_version_id = %q", got)
	}
	adopted := p.apply(typeName, imported, rotated)
	if !adopted.Equal(state) {
		t.Fatalf("state after import and apply = %s, want %s", adopted, state)
	}
	if puts := rec.with(http.MethodPut); len(puts) != 3 || puts[2].Target != "/v1/team/secrets/NPM_TOKEN?id="+lesSecretID {
		t.Fatalf("the first apply after import should send the value to the imported version: %#v", puts)
	}

	p.destroy(typeName, state)
	if deletes := rec.with(http.MethodDelete); len(deletes) != 1 || deletes[0].Target != "/v1/team/secrets/NPM_TOKEN?id="+lesSecretID {
		t.Fatalf("deletes = %#v", deletes)
	}
	if gone := p.read(typeName, state); !gone.IsNull() {
		t.Fatalf("a secret deleted outside Terraform should leave state: %s", gone)
	}
}
