package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// lpgUnknown returns the raw value of model with the values at paths replaced by unknown values.
func lpgUnknown(t *testing.T, ctx context.Context, sch schema.Schema, model any, paths ...path.Path) tftypes.Value {
	t.Helper()
	raw := mustState(t, ctx, sch, model).Raw
	for _, p := range paths {
		target := tftypes.NewAttributePath()
		for _, step := range p.Steps() {
			switch step := step.(type) {
			case path.PathStepAttributeName:
				target = target.WithAttributeName(string(step))
			case path.PathStepElementKeyInt:
				target = target.WithElementKeyInt(int(step))
			case path.PathStepElementKeyString:
				target = target.WithElementKeyString(string(step))
			default:
				t.Fatalf("unsupported path step %T", step)
			}
		}
		replaced := false
		next, err := tftypes.Transform(raw, func(at *tftypes.AttributePath, value tftypes.Value) (tftypes.Value, error) {
			if !at.Equal(target) {
				return value, nil
			}
			replaced = true
			return tftypes.NewValue(value.Type(), tftypes.UnknownValue), nil
		})
		if err != nil || !replaced {
			t.Fatalf("no value at %s: %v", p, err)
		}
		raw = next
	}
	return raw
}

func lpgErrorPaths(diags diag.Diagnostics) []string {
	var out []string
	for _, d := range diags.Errors() {
		if withPath, ok := d.(diag.DiagnosticWithPath); ok {
			out = append(out, withPath.Path().String())
		} else {
			out = append(out, "<no path>")
		}
	}
	return out
}

func lpgWantPaths(t *testing.T, diags diag.Diagnostics, want ...string) {
	t.Helper()
	got := lpgErrorPaths(diags)
	for _, p := range want {
		found := false
		for _, g := range got {
			found = found || g == p
		}
		if !found {
			t.Errorf("no error at %s; error paths %v: %s", p, got, fmtDiag(diags))
		}
	}
	if len(got) != len(want) {
		t.Errorf("error paths = %v, want %v: %s", got, want, fmtDiag(diags))
	}
}

type lpgPoolServer struct {
	*httptest.Server
	list       string
	listStatus int
	delStatus  int
	methods    []string
	posted     string
	deleted    string
}

func newLpgPoolServer(t *testing.T) *lpgPoolServer {
	t.Helper()
	s := &lpgPoolServer{list: `{"pools":[]}`}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.methods = append(s.methods, r.Method)
		switch r.Method {
		case http.MethodPost:
			s.posted = string(raw)
			_, _ = io.WriteString(w, `{"registered":true}`)
		case http.MethodDelete:
			s.deleted = r.URL.RawQuery
			if s.delStatus != 0 {
				w.WriteHeader(s.delStatus)
				_, _ = io.WriteString(w, `{"error":{"code":"x","message":"boom"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"deregistered":true}`)
		default:
			if s.listStatus != 0 {
				w.WriteHeader(s.listStatus)
				_, _ = io.WriteString(w, `{"error":{"code":"x","message":"boom"}}`)
				return
			}
			_, _ = io.WriteString(w, s.list)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func lpgRepoPool() privateWorkerPoolModel {
	return privateWorkerPoolModel{
		Scope: types.StringValue("team"), PoolName: types.StringValue("gpu"),
		RepoOwner: types.StringValue("acme"), RepoName: types.StringValue("api"),
		RepoURL:                   types.StringNull(),
		WorkerReadyTimeoutSeconds: types.Int64Value(0),
	}
}

func TestPoolValidateConfigWaitsForUnknownValues(t *testing.T) {
	ctx := context.Background()
	res := &privateWorkerPoolResource{}
	sch := resourceSchema(t, res)
	validate := func(raw tftypes.Value) diag.Diagnostics {
		resp := &resource.ValidateConfigResponse{}
		res.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: sch, Raw: raw}}, resp)
		return resp.Diagnostics
	}

	model := lpgRepoPool()
	model.RepoURL = types.StringValue("https://github.com/acme/api")
	for _, unknown := range [][]path.Path{
		{path.Root("repo_owner")},
		{path.Root("repo_name")},
		{path.Root("repo_owner"), path.Root("repo_name"), path.Root("repo_url"), path.Root("worker_ready_timeout_seconds")},
	} {
		if diags := validate(lpgUnknown(t, ctx, sch, &model, unknown...)); diags.HasError() {
			t.Errorf("unknown %v should wait: %s", unknown, fmtDiag(diags))
		}
	}

	ownerOnly := lpgRepoPool()
	ownerOnly.RepoName = types.StringNull()
	lpgWantPaths(t, validate(mustState(t, ctx, sch, &ownerOnly).Raw), "repo_name")

	urlOnly := lpgRepoPool()
	urlOnly.RepoOwner, urlOnly.RepoName = types.StringNull(), types.StringNull()
	urlOnly.RepoURL = types.StringValue("https://github.com/acme/api")
	lpgWantPaths(t, validate(mustState(t, ctx, sch, &urlOnly).Raw), "repo_url")

	negative := lpgRepoPool()
	negative.WorkerReadyTimeoutSeconds = types.Int64Value(-1)
	lpgWantPaths(t, validate(mustState(t, ctx, sch, &negative).Raw), "worker_ready_timeout_seconds")

	name := sch.Attributes["pool_name"].(schema.StringAttribute)
	vresp := &validator.StringResponse{}
	for _, v := range name.Validators {
		v.ValidateString(ctx, validator.StringRequest{Path: path.Root("pool_name"), ConfigValue: types.StringValue("")}, vresp)
	}
	if !vresp.Diagnostics.HasError() {
		t.Error("an empty pool_name should be rejected")
	}
}

func TestPoolModifyPlanHandlesNilClientDestroyAndUnknown(t *testing.T) {
	ctx := context.Background()
	sch := resourceSchema(t, &privateWorkerPoolResource{})
	model := lpgRepoPool()
	plan := mustPlan(t, ctx, sch, &model)
	guarded := &privateWorkerPoolResource{client: &apiClient{guardrails: &guardrailsConfig{HasAllowedOwners: true, AllowedOwners: []string{"Other"}}}}

	for name, res := range map[string]*privateWorkerPoolResource{
		"nil client":    {},
		"no guardrails": {client: &apiClient{}},
	} {
		resp := &resource.ModifyPlanResponse{Plan: plan}
		res.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan}, resp)
		if resp.Diagnostics.HasError() {
			t.Errorf("%s: %s", name, fmtDiag(resp.Diagnostics))
		}
	}

	destroy := tfsdk.Plan{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
	resp := &resource.ModifyPlanResponse{Plan: destroy}
	guarded.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: destroy, State: mustState(t, ctx, sch, &model)}, resp)
	if resp.Diagnostics.HasError() {
		t.Errorf("destroy plan was blocked: %s", fmtDiag(resp.Diagnostics))
	}

	unknown := tfsdk.Plan{Schema: sch, Raw: lpgUnknown(t, ctx, sch, &model, path.Root("repo_owner"))}
	resp = &resource.ModifyPlanResponse{Plan: unknown}
	guarded.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: unknown}, resp)
	if resp.Diagnostics.HasError() {
		t.Errorf("unknown repo_owner should wait: %s", fmtDiag(resp.Diagnostics))
	}

	resp = &resource.ModifyPlanResponse{Plan: plan}
	guarded.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan}, resp)
	lpgWantPaths(t, resp.Diagnostics, "repo_owner")

	model.RepoOwner = types.StringValue("OTHER")
	plan = mustPlan(t, ctx, sch, &model)
	resp = &resource.ModifyPlanResponse{Plan: plan}
	guarded.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan}, resp)
	if resp.Diagnostics.HasError() {
		t.Errorf("owner comparison should ignore case: %s", fmtDiag(resp.Diagnostics))
	}
}

func TestPoolCreateRecordsStateWhenReadAfterRegisterFails(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(*lpgPoolServer){
		"list error":   func(s *lpgPoolServer) { s.listStatus = http.StatusInternalServerError },
		"not yet seen": func(s *lpgPoolServer) {},
	} {
		t.Run(name, func(t *testing.T) {
			server := newLpgPoolServer(t)
			setup(server)
			res := &privateWorkerPoolResource{client: poolClient(server.URL, "key_pool")}
			sch := resourceSchema(t, res)
			plan := lpgRepoPool()
			planned := tfsdk.Plan{Schema: sch, Raw: lpgUnknown(t, ctx, sch, &plan, path.Root("id"), path.Root("connected_worker_count"), path.Root("in_use_worker_count"))}
			resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
			res.Create(ctx, resource.CreateRequest{Plan: planned}, resp)
			if !resp.Diagnostics.HasError() {
				t.Fatal("create should report the failed read")
			}
			if resp.State.Raw.IsNull() {
				t.Fatal("a registered pool must be recorded in state")
			}
			if !resp.State.Raw.IsFullyKnown() {
				t.Fatalf("state has unknown values: %s", resp.State.Raw)
			}
			var got privateWorkerPoolModel
			if diags := resp.State.Get(ctx, &got); diags.HasError() {
				t.Fatal(diags)
			}
			if got.ID.ValueString() != "team/gpu/acme/api" || !got.ConnectedWorkerCount.IsNull() {
				t.Fatalf("partial state = %#v", got)
			}
			del := &resource.DeleteResponse{}
			res.Delete(ctx, resource.DeleteRequest{State: resp.State}, del)
			if del.Diagnostics.HasError() || !strings.Contains(server.deleted, "repo_owner=acme") {
				t.Fatalf("partial state should deregister: %q %s", server.deleted, fmtDiag(del.Diagnostics))
			}
		})
	}
}

func TestPoolCreateKeepsPlannedValuesWhenAPIDiffers(t *testing.T) {
	ctx := context.Background()
	server := newLpgPoolServer(t)
	server.list = `{"pools":[{"scope":"team_pool","poolName":"gpu","repoOwner":"Acme","repoName":"API","repoUrl":"https://github.com/acme/other","workerReadyTimeoutSeconds":30,"connectedWorkerCount":2,"inUseWorkerCount":1}]}`
	res := &privateWorkerPoolResource{client: poolClient(server.URL, "key_pool")}
	sch := resourceSchema(t, res)
	plan := lpgRepoPool()
	plan.RepoURL = types.StringValue("https://github.com/acme/api")
	plan.WorkerReadyTimeoutSeconds = types.Int64Value(5)
	resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(fmtDiag(resp.Diagnostics))
	}
	for _, needle := range []string{`"workerReadyTimeoutSeconds":5`, `"repoUrl":"https://github.com/acme/api"`, `"repoOwner":"acme"`, `"repoName":"api"`, `"scope":"team"`, `"poolName":"gpu"`} {
		if !strings.Contains(server.posted, needle) {
			t.Errorf("register body %s lacks %s", server.posted, needle)
		}
	}
	var got privateWorkerPoolModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.WorkerReadyTimeoutSeconds.ValueInt64() != 5 || got.RepoURL.ValueString() != "https://github.com/acme/api" || got.RepoOwner.ValueString() != "acme" || got.ConnectedWorkerCount.ValueInt64() != 2 || got.InUseWorkerCount.ValueInt64() != 1 {
		t.Fatalf("state after create must match the plan: %#v", got)
	}
	if n := resp.Diagnostics.WarningsCount(); n != 2 {
		t.Fatalf("want a warning for the timeout and for repo_url, got %d", n)
	}

	read := &resource.ReadResponse{State: resp.State}
	res.Read(ctx, resource.ReadRequest{State: resp.State}, read)
	if read.Diagnostics.HasError() {
		t.Fatal(fmtDiag(read.Diagnostics))
	}
	read.State.Get(ctx, &got)
	if got.WorkerReadyTimeoutSeconds.ValueInt64() != 30 || got.RepoURL.ValueString() != "https://github.com/acme/other" || got.RepoOwner.ValueString() != "acme" {
		t.Fatalf("refresh should show drift and keep the configured owner casing: %#v", got)
	}
}

func TestPoolReadRemovesAbsentPoolButNotOnListFailure(t *testing.T) {
	ctx := context.Background()
	server := newLpgPoolServer(t)
	res := &privateWorkerPoolResource{client: poolClient(server.URL, "key_pool")}
	sch := resourceSchema(t, res)
	model := lpgRepoPool()
	model.ID = types.StringValue("team/gpu/acme/api")
	model.ConnectedWorkerCount, model.InUseWorkerCount = types.Int64Value(0), types.Int64Value(0)
	state := mustState(t, ctx, sch, &model)

	server.list = `{"pools":[{"scope":"team","poolName":"gpu","workerReadyTimeoutSeconds":0,"connectedWorkerCount":1,"inUseWorkerCount":0}]}`
	read := &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, read)
	if read.Diagnostics.HasError() || !read.State.Raw.IsNull() {
		t.Fatalf("a pool absent from the list should leave state: %s", fmtDiag(read.Diagnostics))
	}

	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		server.listStatus = status
		read = &resource.ReadResponse{State: state}
		res.Read(ctx, resource.ReadRequest{State: state}, read)
		if !read.Diagnostics.HasError() || read.State.Raw.IsNull() {
			t.Fatalf("HTTP %d from the list endpoint must fail the read and keep state", status)
		}
	}
}

func TestPoolImportReadsEveryAttribute(t *testing.T) {
	ctx := context.Background()
	server := newLpgPoolServer(t)
	server.list = `{"pools":[{"scope":"team","poolName":"gpu","repoOwner":"acme","repoName":"api","repoUrl":"https://github.com/acme/api","workerReadyTimeoutSeconds":30,"connectedWorkerCount":1,"inUseWorkerCount":0},{"scope":"team","poolName":"any","repoUrl":"https://github.com/acme/stray","workerReadyTimeoutSeconds":0,"connectedWorkerCount":0,"inUseWorkerCount":0}]}`
	res := &privateWorkerPoolResource{client: poolClient(server.URL, "key_pool")}
	sch := resourceSchema(t, res)
	importRead := func(id string) (privateWorkerPoolModel, tfsdk.State) {
		t.Helper()
		imported := &resource.ImportStateResponse{State: emptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: id}, imported)
		if imported.Diagnostics.HasError() {
			t.Fatal(fmtDiag(imported.Diagnostics))
		}
		read := &resource.ReadResponse{State: imported.State}
		res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
		if read.Diagnostics.HasError() {
			t.Fatal(fmtDiag(read.Diagnostics))
		}
		var got privateWorkerPoolModel
		if !read.State.Raw.IsNull() {
			if diags := read.State.Get(ctx, &got); diags.HasError() {
				t.Fatal(diags)
			}
		}
		return got, read.State
	}

	got, state := importRead("team/gpu/acme/api")
	want := lpgRepoPool()
	want.ID = types.StringValue("team/gpu/acme/api")
	want.RepoURL = types.StringValue("https://github.com/acme/api")
	want.WorkerReadyTimeoutSeconds = types.Int64Value(30)
	want.ConnectedWorkerCount, want.InUseWorkerCount = types.Int64Value(1), types.Int64Value(0)
	if got != want {
		t.Fatalf("imported state = %#v, want %#v", got, want)
	}

	// A later refresh keeps a URL the configuration omits, so the import-only
	// behaviour cannot turn into a perpetual diff.
	want.RepoURL = types.StringNull()
	omitted := mustState(t, ctx, sch, &want)
	read := &resource.ReadResponse{State: omitted}
	res.Read(ctx, resource.ReadRequest{State: omitted}, read)
	read.State.Get(ctx, &got)
	if !got.RepoURL.IsNull() {
		t.Fatalf("refresh filled an omitted repo_url with %s", got.RepoURL)
	}

	// The schema's own rule is that repo_url requires repo_owner and repo_name.
	got, state = importRead("team/any")
	if !got.RepoURL.IsNull() || !got.RepoOwner.IsNull() || !got.RepoName.IsNull() || got.ID.ValueString() != "team/any" {
		t.Fatalf("imported any-repo pool = %#v", got)
	}
	validate := &resource.ValidateConfigResponse{}
	res.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: sch, Raw: state.Raw}}, validate)
	if validate.Diagnostics.HasError() {
		t.Fatalf("imported state violates the resource's validation: %s", fmtDiag(validate.Diagnostics))
	}

	if _, state = importRead("user/missing"); !state.Raw.IsNull() {
		t.Fatal("importing a pool that is not registered should leave no state, which Terraform reports as a missing remote object")
	}
}

func TestPoolImportIDFormat(t *testing.T) {
	ctx := context.Background()
	res := &privateWorkerPoolResource{}
	sch := resourceSchema(t, res)
	for _, id := range []string{"", "team", "org/gpu", "team/", "team//acme/api", "team/gpu/acme", "team/gpu/acme/", "team/gpu/acme/api/extra"} {
		resp := &resource.ImportStateResponse{State: emptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: id}, resp)
		if text := fmtDiag(resp.Diagnostics); !strings.Contains(text, "<scope>/<pool_name>") {
			t.Errorf("import id %q: want a format error, got %q", id, text)
		}
	}

	model := privateWorkerPoolModel{
		Scope: types.StringValue("user"), PoolName: types.StringValue("ml/gpu 100%"),
		RepoOwner: types.StringValue("acme"), RepoName: types.StringValue("api"),
	}
	id := poolTerraformID(model)
	if id != "user/ml%2Fgpu 100%25/acme/api" {
		t.Fatalf("id = %s", id)
	}
	resp := &resource.ImportStateResponse{State: emptyState(ctx, sch)}
	res.ImportState(ctx, resource.ImportStateRequest{ID: id}, resp)
	var got privateWorkerPoolModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() || resp.Diagnostics.HasError() {
		t.Fatal(diags, resp.Diagnostics)
	}
	if got.PoolName.ValueString() != "ml/gpu 100%" || got.Scope.ValueString() != "user" || got.RepoOwner.ValueString() != "acme" || got.RepoName.ValueString() != "api" || got.ID.ValueString() != id {
		t.Fatalf("round trip = %#v", got)
	}
	if plain := poolTerraformID(lpgRepoPool()); plain != "team/gpu/acme/api" {
		t.Fatalf("plain id = %s", plain)
	}
}

func TestPoolRepoURLRespellingUpdatesStateWithoutReplacing(t *testing.T) {
	ctx := context.Background()
	server := newLpgPoolServer(t)
	server.list = `{"pools":[{"scope":"team","poolName":"gpu","repoOwner":"acme","repoName":"api","repoUrl":"https://github.com/acme/api","workerReadyTimeoutSeconds":0,"connectedWorkerCount":3,"inUseWorkerCount":2}]}`
	res := &privateWorkerPoolResource{client: poolClient(server.URL, "key_pool")}
	sch := resourceSchema(t, res)
	prior := lpgRepoPool()
	prior.ID = types.StringValue("team/gpu/acme/api")
	prior.RepoURL = types.StringValue("https://github.com/acme/api")
	prior.ConnectedWorkerCount, prior.InUseWorkerCount = types.Int64Value(1), types.Int64Value(0)
	state := mustState(t, ctx, sch, &prior)

	modifier := sch.Attributes["repo_url"].(schema.StringAttribute).PlanModifiers[0]
	replaces := func(planValue types.String) bool {
		next := prior
		next.RepoURL = planValue
		resp := &planmodifier.StringResponse{PlanValue: planValue}
		modifier.PlanModifyString(ctx, planmodifier.StringRequest{
			State: state, Plan: mustPlan(t, ctx, sch, &next), StateValue: prior.RepoURL, PlanValue: planValue,
		}, resp)
		return resp.RequiresReplace
	}
	for value, want := range map[string]bool{
		"https://github.com/acme/api":   false,
		"github.com/Acme/api.git":       false,
		"https://github.com/acme/other": true,
	} {
		if got := replaces(types.StringValue(value)); got != want {
			t.Errorf("repo_url %s: replace = %t, want %t", value, got, want)
		}
	}
	if !replaces(types.StringNull()) || !replaces(types.StringUnknown()) {
		t.Error("removing repo_url or an unknown repo_url must replace the pool")
	}

	plan := prior
	plan.RepoURL = types.StringValue("github.com/Acme/api.git")
	planned := tfsdk.Plan{Schema: sch, Raw: lpgUnknown(t, ctx, sch, &plan, path.Root("connected_worker_count"), path.Root("in_use_worker_count"))}
	update := &resource.UpdateResponse{State: state}
	res.Update(ctx, resource.UpdateRequest{Plan: planned, State: state}, update)
	if update.Diagnostics.HasError() {
		t.Fatal(fmtDiag(update.Diagnostics))
	}
	var got privateWorkerPoolModel
	update.State.Get(ctx, &got)
	if got.RepoURL.ValueString() != "github.com/Acme/api.git" || got.ConnectedWorkerCount.ValueInt64() != 3 || got.InUseWorkerCount.ValueInt64() != 2 || got.ID.ValueString() != "team/gpu/acme/api" {
		t.Fatalf("updated state = %#v", got)
	}
	for _, method := range server.methods {
		if method != http.MethodGet {
			t.Fatalf("update sent %s", method)
		}
	}

	plan.WorkerReadyTimeoutSeconds = types.Int64Value(9)
	update = &resource.UpdateResponse{State: state}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: state}, update)
	if !update.Diagnostics.HasError() {
		t.Fatal("a change that needs replacement must not be accepted as an update")
	}
}

func TestPoolDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	server := newLpgPoolServer(t)
	res := &privateWorkerPoolResource{client: poolClient(server.URL, "key_pool")}
	sch := resourceSchema(t, res)
	model := lpgRepoPool()
	model.RepoOwner, model.RepoName = types.StringNull(), types.StringNull()
	state := mustState(t, ctx, sch, &model)

	server.delStatus = http.StatusNotFound
	del := &resource.DeleteResponse{}
	res.Delete(ctx, resource.DeleteRequest{State: state}, del)
	if del.Diagnostics.HasError() {
		t.Fatalf("an already deregistered pool should delete cleanly: %s", fmtDiag(del.Diagnostics))
	}
	if strings.Contains(server.deleted, "repo_owner") || !strings.Contains(server.deleted, "scope=team") {
		t.Fatalf("delete query = %s", server.deleted)
	}

	server.delStatus = http.StatusInternalServerError
	del = &resource.DeleteResponse{}
	res.Delete(ctx, resource.DeleteRequest{State: state}, del)
	if !del.Diagnostics.HasError() {
		t.Fatal("a failed deregister must be reported")
	}
}

func lpgWorkflow(ctx context.Context) platformWorkflowModel {
	repos, _ := types.ListValueFrom(ctx, types.StringType, []string{"acme/api"})
	labels, _ := types.MapValueFrom(ctx, types.StringType, map[string]string{"pool": "gpu"})
	return platformWorkflowModel{
		Name:                 types.StringValue("review"),
		Prompt:               types.StringValue("review"),
		Scope:                types.StringValue("team"),
		Model:                types.StringValue("composer-2"),
		GitRepo:              types.StringNull(),
		DisabledDefaultTools: types.ListNull(types.StringType),
		PrivateWorker:        &privateWorkerModel{Labels: labels},
		Triggers: []triggerModel{{
			GitPullRequest: &gitPullRequestModel{Repos: repos, Orgs: types.ListNull(types.StringType)},
			UserAllowlist:  types.ListNull(types.StringType),
		}},
		Actions: []actionModel{{
			PrComment: &prCommentActionModel{AllowInlineComments: types.BoolNull(), AllowApprove: types.BoolValue(false)},
		}},
	}
}

func lpgAllGuardrails() *platformWorkflowResource {
	return &platformWorkflowResource{client: &apiClient{guardrails: &guardrailsConfig{
		HasAllowedScopes: true, AllowedScopes: []string{"team"},
		RequirePoolLabel: true,
		HasAllowedModels: true, AllowedModels: []string{"composer-2"},
		HasAllowedOwners: true, AllowedOwners: []string{"acme"},
		DenyPRApproval: true,
	}}}
}

func lpgModifyPlan(ctx context.Context, res *platformWorkflowResource, sch schema.Schema, config, plan tftypes.Value, state *tftypes.Value) diag.Diagnostics {
	req := resource.ModifyPlanRequest{
		Config: tfsdk.Config{Schema: sch, Raw: config},
		Plan:   tfsdk.Plan{Schema: sch, Raw: plan},
	}
	if state != nil {
		req.State = tfsdk.State{Schema: sch, Raw: *state}
	}
	resp := &resource.ModifyPlanResponse{Plan: req.Plan}
	res.ModifyPlan(ctx, req, resp)
	return resp.Diagnostics
}

func TestGuardrailsAcceptCompliantAutomationAndNeverBlockDestroy(t *testing.T) {
	ctx := context.Background()
	res := lpgAllGuardrails()
	sch := resourceSchema(t, res)
	model := lpgWorkflow(ctx)
	raw := mustState(t, ctx, sch, &model).Raw
	if diags := lpgModifyPlan(ctx, res, sch, raw, raw, nil); len(diags) != 0 {
		t.Fatalf("compliant automation: %v", diags)
	}

	bad := lpgWorkflow(ctx)
	bad.Scope, bad.Model, bad.PrivateWorker = types.StringValue("user"), types.StringValue("gpt-4o"), nil
	badRaw := mustState(t, ctx, sch, &bad).Raw
	null := tftypes.NewValue(sch.Type().TerraformType(ctx), nil)
	if diags := lpgModifyPlan(ctx, res, sch, null, null, &badRaw); len(diags) != 0 {
		t.Fatalf("destroying a non-compliant automation was blocked: %v", diags)
	}

	for name, unguarded := range map[string]*platformWorkflowResource{
		"nil client":     {},
		"nil guardrails": {client: &apiClient{}},
		"empty block":    {client: &apiClient{guardrails: &guardrailsConfig{}}},
	} {
		if diags := lpgModifyPlan(ctx, unguarded, sch, badRaw, badRaw, nil); len(diags) != 0 {
			t.Errorf("%s: %v", name, diags)
		}
	}
}

func TestGuardrailErrorsNameTheAttributePath(t *testing.T) {
	ctx := context.Background()
	res := lpgAllGuardrails()
	sch := resourceSchema(t, res)
	model := lpgWorkflow(ctx)
	model.Scope = types.StringValue("user")
	model.Model = types.StringValue("gpt-4o")
	model.GitRepo = types.StringValue("github.com/evil/tool")
	labels, _ := types.MapValueFrom(ctx, types.StringType, map[string]string{"repo": "acme/api", "pool": " "})
	model.PrivateWorker = &privateWorkerModel{Labels: labels}
	repos, _ := types.ListValueFrom(ctx, types.StringType, []string{"acme/api", "https://github.com/evil/api.git"})
	orgs, _ := types.ListValueFrom(ctx, types.StringType, []string{"evil"})
	model.Triggers = []triggerModel{
		{GitPullRequest: &gitPullRequestModel{Repos: repos, Orgs: types.ListNull(types.StringType)}, UserAllowlist: types.ListNull(types.StringType)},
		{GitPullRequest: &gitPullRequestModel{Repos: types.ListNull(types.StringType), Orgs: orgs}, UserAllowlist: types.ListNull(types.StringType)},
		{GitPush: &gitPushModel{Repo: types.StringValue("git@github.com:evil/api.git")}, UserAllowlist: types.ListNull(types.StringType)},
		{GitCICompleted: &gitCICompletedModel{Repos: orgs}, UserAllowlist: types.ListNull(types.StringType)},
		{GitLabel: &gitLabelModel{Repos: orgs}, UserAllowlist: types.ListNull(types.StringType)},
	}
	model.Actions = []actionModel{
		{PrComment: &prCommentActionModel{AllowInlineComments: types.BoolNull(), AllowApprove: types.BoolValue(true)}},
		{ApprovePr: &approvePrActionModel{}},
	}
	raw := mustState(t, ctx, sch, &model).Raw
	lpgWantPaths(t, lpgModifyPlan(ctx, res, sch, raw, raw, nil),
		"scope",
		"model",
		"git_repo",
		`private_worker.labels["repo"]`,
		"private_worker.labels",
		"trigger[0].git_pull_request.repos[1]",
		"trigger[1].git_pull_request.orgs[0]",
		"trigger[2].git_push.repo",
		"trigger[3].git_ci_completed.repos[0]",
		"trigger[4].git_label.repos[0]",
		"action[0].pr_comment.allow_approve",
		"action[1].approve_pr",
	)

	selection := lpgWorkflow(ctx)
	selection.Model = types.StringNull()
	selection.ModelSelection = &modelSelectionModel{ModelID: types.StringValue("auto-smart"), MaxMode: types.BoolNull()}
	selection.PrivateWorker = nil
	raw = mustState(t, ctx, sch, &selection).Raw
	lpgWantPaths(t, lpgModifyPlan(ctx, res, sch, raw, raw, nil), "model_selection.model_id", "private_worker")
}

func TestGuardrailsWaitForUnknownNestedValues(t *testing.T) {
	ctx := context.Background()
	res := lpgAllGuardrails()
	sch := resourceSchema(t, res)
	model := lpgWorkflow(ctx)
	for _, unknown := range [][]path.Path{
		{path.Root("private_worker")},
		{path.Root("private_worker").AtName("labels")},
		{path.Root("private_worker").AtName("labels").AtMapKey("pool")},
		{path.Root("model"), path.Root("model_selection")},
		{path.Root("trigger")},
		{path.Root("trigger").AtListIndex(0)},
		{path.Root("trigger").AtListIndex(0).AtName("git_pull_request")},
		{path.Root("trigger").AtListIndex(0).AtName("git_pull_request").AtName("repos")},
		{path.Root("trigger").AtListIndex(0).AtName("git_pull_request").AtName("repos").AtListIndex(0)},
		{path.Root("git_repo")},
		{path.Root("action")},
		{path.Root("action").AtListIndex(0)},
		{path.Root("action").AtListIndex(0).AtName("pr_comment")},
		{path.Root("action").AtListIndex(0).AtName("pr_comment").AtName("allow_approve")},
		{path.Root("scope")},
	} {
		raw := lpgUnknown(t, ctx, sch, &model, unknown...)
		if diags := lpgModifyPlan(ctx, res, sch, raw, raw, nil); len(diags) != 0 {
			t.Errorf("unknown %v should wait without a diagnostic: %v", unknown, diags)
		}
	}

	selection := lpgWorkflow(ctx)
	selection.Model = types.StringNull()
	selection.ModelSelection = &modelSelectionModel{ModelID: types.StringValue("composer-2"), MaxMode: types.BoolNull()}
	raw := lpgUnknown(t, ctx, sch, &selection, path.Root("model_selection").AtName("model_id"))
	if diags := lpgModifyPlan(ctx, res, sch, raw, raw, nil); len(diags) != 0 {
		t.Errorf("unknown model_id should wait: %v", diags)
	}
}

func TestGuardrailsCheckKnownValuesBesideUnknownOnes(t *testing.T) {
	ctx := context.Background()
	res := lpgAllGuardrails()
	sch := resourceSchema(t, res)
	model := lpgWorkflow(ctx)
	evil, _ := types.ListValueFrom(ctx, types.StringType, []string{"evil/api", "acme/api"})
	model.Triggers = append(model.Triggers, triggerModel{
		GitCICompleted: &gitCICompletedModel{Repos: evil}, UserAllowlist: types.ListNull(types.StringType),
	})
	labels, _ := types.MapValueFrom(ctx, types.StringType, map[string]string{"pool": "gpu", "repo": "x"})
	model.PrivateWorker = &privateWorkerModel{Labels: labels}
	raw := lpgUnknown(t, ctx, sch, &model,
		path.Root("trigger").AtListIndex(0).AtName("git_pull_request").AtName("repos"),
		path.Root("trigger").AtListIndex(1).AtName("git_ci_completed").AtName("repos").AtListIndex(1),
		path.Root("private_worker").AtName("labels").AtMapKey("pool"),
		path.Root("private_worker").AtName("labels").AtMapKey("repo"),
	)
	lpgWantPaths(t, lpgModifyPlan(ctx, res, sch, raw, raw, nil),
		"trigger[1].git_ci_completed.repos[0]",
		`private_worker.labels["repo"]`,
	)
}

func TestGuardrailsRejectRepositoryWithNoDeterminableOwner(t *testing.T) {
	ctx := context.Background()
	res := lpgAllGuardrails()
	sch := resourceSchema(t, res)
	model := lpgWorkflow(ctx)
	model.GitRepo = types.StringValue("https://github.com")
	raw := mustState(t, ctx, sch, &model).Raw
	diags := lpgModifyPlan(ctx, res, sch, raw, raw, nil)
	lpgWantPaths(t, diags, "git_repo")
	if !strings.Contains(fmtDiag(diags), "cannot determine the owner") {
		t.Fatal(fmtDiag(diags))
	}
	g := res.client.guardrails
	if g.checkOwners("environment repository", []string{"github.com"}).ErrorsCount() != 1 || g.checkOwners("environment repository", []string{"", "  ", "ACME/api"}).HasError() {
		t.Fatal("checkOwners should reject an undeterminable owner and skip blank values")
	}
}

func TestGuardrailScopeOnCreateUpdateAndImport(t *testing.T) {
	ctx := context.Background()
	res := &platformWorkflowResource{client: &apiClient{guardrails: &guardrailsConfig{HasAllowedScopes: true, AllowedScopes: []string{" Team "}}}}
	sch := resourceSchema(t, res)
	omitted := lpgWorkflow(ctx)
	omitted.Scope = types.StringNull()
	config := mustState(t, ctx, sch, &omitted).Raw
	withScope := func(scope types.String) tftypes.Value {
		m := omitted
		m.Scope = scope
		m.ID = types.StringValue("11111111-1111-1111-1111-111111111111")
		return mustState(t, ctx, sch, &m).Raw
	}

	create := lpgUnknown(t, ctx, sch, &omitted, path.Root("scope"), path.Root("id"))
	diags := lpgModifyPlan(ctx, res, sch, config, create, nil)
	lpgWantPaths(t, diags, "scope")
	if !strings.Contains(fmtDiag(diags), `scope "user"`) {
		t.Fatalf("create with an omitted scope is user: %s", fmtDiag(diags))
	}

	team := withScope(types.StringValue("team"))
	if diags := lpgModifyPlan(ctx, res, sch, config, team, &team); len(diags) != 0 {
		t.Fatalf("update with an omitted scope keeps the prior team scope: %v", diags)
	}

	// An imported user-scope automation whose configuration matches it produces a
	// plan with no changes. It still fails, and says the automation already exists.
	user := withScope(types.StringValue("user"))
	diags = lpgModifyPlan(ctx, res, sch, config, user, &user)
	lpgWantPaths(t, diags, "scope")
	if !strings.Contains(fmtDiag(diags), `scope "user"`) || !strings.Contains(fmtDiag(diags), "keeps the prior value") || diags.WarningsCount() != 1 {
		t.Fatalf("unchanged non-compliant automation: %v", diags)
	}
	if diags := lpgModifyPlan(ctx, res, sch, config, user, &team); diags.WarningsCount() != 0 || !diags.HasError() {
		t.Fatalf("a plan that changes the automation gets the error without the existing-automation warning: %v", diags)
	}

	for name, scope := range map[string]types.String{"null": types.StringNull(), "unknown": types.StringUnknown()} {
		plan := withScope(scope)
		diags = lpgModifyPlan(ctx, res, sch, config, plan, &team)
		lpgWantPaths(t, diags, "scope")
		if !strings.Contains(fmtDiag(diags), "Set scope explicitly") {
			t.Fatalf("%s planned scope on update: %s", name, fmtDiag(diags))
		}
	}

	explicit := lpgWorkflow(ctx)
	explicit.Scope = types.StringValue("TEAM")
	raw := mustState(t, ctx, sch, &explicit).Raw
	if diags := lpgModifyPlan(ctx, res, sch, raw, raw, nil); len(diags) != 0 {
		t.Fatalf("scope comparison should ignore case and padding: %v", diags)
	}
	explicit.Scope = types.StringValue("team_editable")
	raw = mustState(t, ctx, sch, &explicit).Raw
	lpgWantPaths(t, lpgModifyPlan(ctx, res, sch, raw, raw, nil), "scope")
}

func TestGuardrailApprovalReadsThePlannedValue(t *testing.T) {
	ctx := context.Background()
	res := &platformWorkflowResource{client: &apiClient{guardrails: &guardrailsConfig{DenyPRApproval: true}}}
	sch := resourceSchema(t, res)
	configured := lpgWorkflow(ctx)
	configured.Actions[0].PrComment.AllowApprove = types.BoolNull()
	config := mustState(t, ctx, sch, &configured).Raw

	prior := lpgWorkflow(ctx)
	prior.Actions = []actionModel{{PrComment: &prCommentActionModel{AllowInlineComments: types.BoolNull(), AllowApprove: types.BoolValue(true)}}}
	state := mustState(t, ctx, sch, &prior).Raw
	diags := lpgModifyPlan(ctx, res, sch, config, state, &state)
	lpgWantPaths(t, diags, "action[0].pr_comment.allow_approve")
	if diags.WarningsCount() != 1 {
		t.Fatalf("want the existing-automation warning: %v", diags)
	}

	changing := lpgUnknown(t, ctx, sch, &configured, path.Root("action").AtListIndex(0).AtName("pr_comment").AtName("allow_approve"))
	if diags := lpgModifyPlan(ctx, res, sch, config, changing, &state); len(diags) != 0 {
		t.Fatalf("an omitted allow_approve that is being recomputed should wait: %v", diags)
	}
}

func TestGuardrailPoolLabelRequiresNonEmptyKnownString(t *testing.T) {
	ctx := context.Background()
	res := &platformWorkflowResource{client: &apiClient{guardrails: &guardrailsConfig{RequirePoolLabel: true}}}
	sch := resourceSchema(t, res)
	for name, labels := range map[string]types.Map{
		"null labels":  types.MapNull(types.StringType),
		"empty labels": types.MapValueMust(types.StringType, nil),
		"other label":  types.MapValueMust(types.StringType, map[string]attr.Value{"gpu": types.StringValue("a100")}),
		"null pool":    types.MapValueMust(types.StringType, map[string]attr.Value{"pool": types.StringNull()}),
		"blank pool":   types.MapValueMust(types.StringType, map[string]attr.Value{"pool": types.StringValue("  ")}),
	} {
		model := lpgWorkflow(ctx)
		model.PrivateWorker = &privateWorkerModel{Labels: labels}
		raw := mustState(t, ctx, sch, &model).Raw
		diags := lpgModifyPlan(ctx, res, sch, raw, raw, nil)
		if got := lpgErrorPaths(diags); len(got) != 1 || got[0] != "private_worker.labels" {
			t.Errorf("%s: error paths %v: %s", name, got, fmtDiag(diags))
		}
	}
}

func TestGuardrailsAreCheckedWhileCredentialsAreUnknown(t *testing.T) {
	ctx := context.Background()
	p, providerSchema := ldsProviderSchema(t)
	registered := map[string]resource.ResourceWithModifyPlan{}
	for _, register := range p.Resources(ctx) {
		res := register()
		meta := &resource.MetadataResponse{}
		res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "cursor"}, meta)
		if planner, ok := res.(resource.ResourceWithModifyPlan); ok {
			registered[meta.TypeName] = planner
		}
	}
	modify := func(name string, model any) diag.Diagnostics {
		res, ok := registered[name]
		if !ok {
			t.Fatalf("%s is not registered with a plan check", name)
		}
		sch := resourceSchema(t, res)
		raw := mustState(t, ctx, sch, model).Raw
		plan := tfsdk.Plan{Schema: sch, Raw: raw}
		resp := &resource.ModifyPlanResponse{Plan: plan}
		res.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, Config: tfsdk.Config{Schema: sch, Raw: raw}, State: emptyState(ctx, sch)}, resp)
		return resp.Diagnostics
	}

	badEnvironment, goodEnvironment := lesEnvironment("https://github.com/evil/tool"), lesEnvironment("https://github.com/acme/api")
	badPool, goodPool := lpgRepoPool(), lpgRepoPool()
	badPool.RepoOwner = types.StringValue("evil")
	badWorkflow, goodWorkflow := lpgWorkflow(ctx), lpgWorkflow(ctx)
	badWorkflow.Scope = types.StringValue("user")
	violating := map[string]any{"cursor_environment": &badEnvironment, "cursor_private_worker_pool": &badPool, "cursor_platform_workflow": &badWorkflow}
	compliant := map[string]any{"cursor_environment": &goodEnvironment, "cursor_private_worker_pool": &goodPool, "cursor_platform_workflow": &goodWorkflow}

	for name, model := range violating {
		if diags := modify(name, model); len(diags) != 0 {
			t.Errorf("%s was checked before the provider had guardrails: %s", name, fmtDiag(diags))
		}
	}

	guardrailsType := providerSchema.Type().TerraformType(ctx).(tftypes.Object).AttributeTypes["guardrails"]
	list := func(values ...string) tftypes.Value {
		elements := make([]tftypes.Value, len(values))
		for i, value := range values {
			elements[i] = tftypes.NewValue(tftypes.String, value)
		}
		return tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, elements)
	}
	resp := ldsConfigure(t, p, providerSchema, false, map[string]tftypes.Value{
		"api_key": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"guardrails": ldsObject(t, guardrailsType, map[string]tftypes.Value{
			"allowed_repo_owners":       list("acme"),
			"allowed_automation_scopes": list("team"),
		}),
	})
	if resp.Diagnostics.HasError() || resp.ResourceData != nil {
		t.Fatalf("configure: %s, resource data %v", fmtDiag(resp.Diagnostics), resp.ResourceData)
	}
	if warnings := resp.Diagnostics.Warnings(); len(warnings) != 1 || !strings.Contains(warnings[0].Detail(), "Guardrails are still checked") {
		t.Fatalf("warnings = %v", warnings)
	}

	for name, model := range violating {
		if diags := modify(name, model); !diags.HasError() {
			t.Errorf("%s: a violating plan passed while the credentials were unknown", name)
		}
	}
	for name, model := range compliant {
		if diags := modify(name, model); len(diags) != 0 {
			t.Errorf("%s: a compliant plan was rejected: %s", name, fmtDiag(diags))
		}
	}
}
