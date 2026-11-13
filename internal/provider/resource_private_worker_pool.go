package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                   = (*privateWorkerPoolResource)(nil)
	_ resource.ResourceWithImportState    = (*privateWorkerPoolResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*privateWorkerPoolResource)(nil)
	_ resource.ResourceWithValidateConfig = (*privateWorkerPoolResource)(nil)
)

type privateWorkerPoolResource struct {
	client   *apiClient
	provider *cursorProvider
}

type privateWorkerPoolModel struct {
	ID                        types.String `tfsdk:"id"`
	Scope                     types.String `tfsdk:"scope"`
	PoolName                  types.String `tfsdk:"pool_name"`
	RepoOwner                 types.String `tfsdk:"repo_owner"`
	RepoName                  types.String `tfsdk:"repo_name"`
	RepoURL                   types.String `tfsdk:"repo_url"`
	WorkerReadyTimeoutSeconds types.Int64  `tfsdk:"worker_ready_timeout_seconds"`
	ConnectedWorkerCount      types.Int64  `tfsdk:"connected_worker_count"`
	InUseWorkerCount          types.Int64  `tfsdk:"in_use_worker_count"`
}

func NewPrivateWorkerPoolResource() resource.Resource {
	return &privateWorkerPoolResource{}
}

func (r *privateWorkerPoolResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_private_worker_pool"
}

func (r *privateWorkerPoolResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "Registers a durable private worker pool. Register returns no id; the Terraform id is team/<pool_name> or team/<pool_name>/<repo_owner>/<repo_name> (user/... when scope is user). Import uses the same id; inside a name, write / as %2F and % as %25. There is no update endpoint, so every argument change replaces the pool and removes it from pickers until the new one is registered. That includes worker_ready_timeout_seconds. The one exception is an equivalent spelling of the same repo_url, which only updates state. Deregister is a soft-delete: the pool leaves pickers, and workers already connected stay connected. Authentication uses pool_api_key, one team service-account key for every pool in the team. Automations select the pool with private_worker.labels.pool. The repo label is reserved. This is a v0 API.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
				Description: "team/<pool_name>, team/<pool_name>/<repo_owner>/<repo_name>, or the same forms with user. Inside a name, / is written %2F and % is written %25.",
			},
			"scope": schema.StringAttribute{
				Required:      true,
				Description:   "team or user. The list filter uses team_pool for team and personal for user.",
				PlanModifiers: replace,
				Validators:    []validator.String{stringvalidator.OneOf("team", "user")},
			},
			"pool_name": schema.StringAttribute{
				Required:      true,
				Description:   "Pool name. Automations reference it as private_worker.labels.pool.",
				PlanModifiers: replace,
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
			},
			"repo_owner": schema.StringAttribute{
				Optional:      true,
				Description:   "Repository owner for a repo-backed pool. Set together with repo_name. Omit both for an any-repo pool, which routes on the pool name alone.",
				PlanModifiers: replace,
			},
			"repo_name": schema.StringAttribute{
				Optional:      true,
				Description:   "Repository name for a repo-backed pool. Set together with repo_owner.",
				PlanModifiers: replace,
			},
			"repo_url": schema.StringAttribute{
				Optional:      true,
				Description:   "Repository URL for display. Requires repo_owner and repo_name. Changing it replaces the pool, except that an equivalent spelling of the same repository only updates state.",
				PlanModifiers: []planmodifier.String{poolRepoURLRequiresReplace{}},
			},
			"worker_ready_timeout_seconds": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(0),
				Description: "Seconds a claimed request waits for an offline worker to reconnect. 0 reacquires from the pool immediately. Changing this replaces the pool.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"connected_worker_count": schema.Int64Attribute{
				Computed:    true,
				Description: "Workers currently connected to this pool.",
			},
			"in_use_worker_count": schema.Int64Attribute{
				Computed:    true,
				Description: "Connected workers that currently have an assigned agent.",
			},
		},
	}
}

func (r *privateWorkerPoolResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*apiClient)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data type", fmt.Sprintf("Expected *apiClient, got %T", req.ProviderData))
		return
	}
	r.client = client
}

func (r *privateWorkerPoolResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg privateWorkerPoolModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// An unknown value may still resolve to a set or an unset value, so a pairing
	// rule is only decided once both sides are known.
	ownerKnown, nameKnown := !cfg.RepoOwner.IsUnknown(), !cfg.RepoName.IsUnknown()
	ownerSet, nameSet, urlSet := optionalString(cfg.RepoOwner) != "", optionalString(cfg.RepoName) != "", optionalString(cfg.RepoURL) != ""
	if ownerKnown && nameKnown && ownerSet != nameSet {
		missing := path.Root("repo_name")
		if nameSet {
			missing = path.Root("repo_owner")
		}
		resp.Diagnostics.AddAttributeError(missing, "Invalid worker pool", "repo_owner and repo_name must be set together.")
	}
	if urlSet && (ownerKnown && !ownerSet || nameKnown && !nameSet) {
		resp.Diagnostics.AddAttributeError(path.Root("repo_url"), "Invalid worker pool", "repo_url requires repo_owner and repo_name.")
	}
	if !cfg.WorkerReadyTimeoutSeconds.IsUnknown() && !cfg.WorkerReadyTimeoutSeconds.IsNull() && cfg.WorkerReadyTimeoutSeconds.ValueInt64() < 0 {
		resp.Diagnostics.AddAttributeError(path.Root("worker_ready_timeout_seconds"), "Invalid worker pool", "worker_ready_timeout_seconds must be zero or greater.")
	}
}

func (r *privateWorkerPoolResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	guardrails := planGuardrails(r.client, r.provider)
	if req.Plan.Raw.IsNull() || guardrails == nil || !guardrails.HasAllowedOwners {
		return
	}
	var owner types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("repo_owner"), &owner)...)
	if resp.Diagnostics.HasError() || optionalString(owner) == "" {
		return
	}
	if !guardrails.ownerAllowed(owner.ValueString()) {
		resp.Diagnostics.AddAttributeError(path.Root("repo_owner"), "Repository owner is not allowed", fmt.Sprintf("guardrails.allowed_repo_owners rejected worker pool repo_owner %q.", owner.ValueString()))
	}
}

func (r *privateWorkerPoolResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan privateWorkerPoolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePools(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	if err := client.RegisterPool(ctx, poolRequest(plan)); err != nil {
		resp.Diagnostics.AddError("Failed to register private worker pool", err.Error())
		return
	}
	state := plan
	state.ID = types.StringValue(poolTerraformID(plan))
	pool, err := findPool(ctx, client, plan)
	if err != nil {
		// The pool is registered. Recording it keeps it under management, so the
		// next apply replaces it instead of leaving a registration nothing tracks.
		state.ConnectedWorkerCount = types.Int64Null()
		state.InUseWorkerCount = types.Int64Null()
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		resp.Diagnostics.AddError("Failed to read private worker pool after registering it", err.Error()+" The pool was registered and is recorded in state; the next apply replaces it.")
		return
	}
	state.ConnectedWorkerCount = types.Int64Value(pool.ConnectedWorkerCount)
	state.InUseWorkerCount = types.Int64Value(pool.InUseWorkerCount)
	// State keeps the planned values, which Terraform requires after apply. A value
	// the API stored differently shows up as drift on the next refresh.
	if pool.WorkerReadyTimeoutSeconds != plan.WorkerReadyTimeoutSeconds.ValueInt64() {
		resp.Diagnostics.AddAttributeWarning(path.Root("worker_ready_timeout_seconds"), "Private worker pool differs from configuration", fmt.Sprintf("The API reports worker_ready_timeout_seconds = %d after registering the pool with %d. The next plan shows the difference.", pool.WorkerReadyTimeoutSeconds, plan.WorkerReadyTimeoutSeconds.ValueInt64()))
	}
	if url := optionalString(plan.RepoURL); url != "" && pool.RepoURL != "" && publicapi.CanonicalRepo(url) != publicapi.CanonicalRepo(pool.RepoURL) {
		resp.Diagnostics.AddAttributeWarning(path.Root("repo_url"), "Private worker pool differs from configuration", fmt.Sprintf("The API reports repo_url %q after registering the pool with %q. The next plan shows the difference.", pool.RepoURL, url))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *privateWorkerPoolResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state privateWorkerPoolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePools(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	pool, err := findPool(ctx, client, state)
	if errors.Is(err, errPoolNotRegistered) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read private worker pool", err.Error())
		return
	}
	// Create and Read always store the timeout, so a null one means this is the
	// first read after import. There is no configured repo_url to preserve yet, and
	// leaving it null would plan a replacement for any configuration that sets it.
	imported := state.WorkerReadyTimeoutSeconds.IsNull()
	state.ID = types.StringValue(poolTerraformID(state))
	state.ConnectedWorkerCount = types.Int64Value(pool.ConnectedWorkerCount)
	state.InUseWorkerCount = types.Int64Value(pool.InUseWorkerCount)
	state.WorkerReadyTimeoutSeconds = types.Int64Value(pool.WorkerReadyTimeoutSeconds)
	if imported {
		if pool.RepoURL != "" && optionalString(state.RepoOwner) != "" {
			state.RepoURL = types.StringValue(pool.RepoURL)
		}
	} else {
		state.RepoURL = keepOptionalRepoURL(state.RepoURL, pool.RepoURL)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update only runs for a repo_url respelling, the one change that does not force
// replacement. Nothing is sent to the API.
func (r *privateWorkerPoolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state privateWorkerPoolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	unchanged := plan.Scope.Equal(state.Scope) && plan.PoolName.Equal(state.PoolName) && plan.RepoOwner.Equal(state.RepoOwner) && plan.RepoName.Equal(state.RepoName) && plan.WorkerReadyTimeoutSeconds.Equal(state.WorkerReadyTimeoutSeconds)
	if !unchanged || !plan.RepoURL.Equal(state.RepoURL) && !poolRepoURLEquivalent(state.RepoURL, plan.RepoURL) {
		resp.Diagnostics.AddError("Private worker pools cannot be updated", "Every argument change forces replacement. There is no update endpoint.")
		return
	}
	client, err := requirePools(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	pool, err := findPool(ctx, client, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read private worker pool", err.Error())
		return
	}
	plan.ID = types.StringValue(poolTerraformID(plan))
	plan.ConnectedWorkerCount = types.Int64Value(pool.ConnectedWorkerCount)
	plan.InUseWorkerCount = types.Int64Value(pool.InUseWorkerCount)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *privateWorkerPoolResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state privateWorkerPoolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePools(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	err = client.DeregisterPool(ctx, state.Scope.ValueString(), state.PoolName.ValueString(), optionalString(state.RepoOwner), optionalString(state.RepoName))
	if err != nil && !publicapi.IsNotFound(err) {
		resp.Diagnostics.AddError("Failed to deregister private worker pool", err.Error())
	}
}

func (r *privateWorkerPoolResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	scope, name, owner, repo, err := parsePoolID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	model := privateWorkerPoolModel{Scope: types.StringValue(scope), PoolName: types.StringValue(name)}
	if owner != "" {
		model.RepoOwner = types.StringValue(owner)
		model.RepoName = types.StringValue(repo)
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), poolTerraformID(model))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("scope"), scope)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("pool_name"), name)...)
	if owner != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("repo_owner"), owner)...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("repo_name"), repo)...)
	}
}

var errPoolNotRegistered = errors.New("the pool is not in the list of registered pools")

// findPool returns errPoolNotRegistered only when the list call succeeded and the
// pool is absent. An HTTP 404 from the list endpoint itself is a failed read, not a
// deregistered pool.
func findPool(ctx context.Context, client *publicapi.Client, model privateWorkerPoolModel) (*publicapi.Pool, error) {
	scope := model.Scope.ValueString()
	pools, err := client.ListPools(ctx, poolListScope(scope))
	if err != nil {
		return nil, err
	}
	wantOwner := optionalString(model.RepoOwner)
	wantRepo := optionalString(model.RepoName)
	for i := range pools {
		pool := &pools[i]
		// The list is already filtered by scope. Repository owners and names are
		// not case-sensitive on the host, so the API may return another casing.
		if poolScopeMatches(pool.Scope, scope) && pool.PoolName == model.PoolName.ValueString() && strings.EqualFold(pool.RepoOwner, wantOwner) && strings.EqualFold(pool.RepoName, wantRepo) {
			return pool, nil
		}
	}
	return nil, errPoolNotRegistered
}

func poolScopeMatches(apiScope, scope string) bool {
	return apiScope == "" || apiScope == scope || apiScope == poolListScope(scope)
}

func poolRepoURLEquivalent(a, b types.String) bool {
	if a.IsNull() || a.IsUnknown() || b.IsNull() || b.IsUnknown() {
		return false
	}
	canonical := publicapi.CanonicalRepo(a.ValueString())
	return canonical != "" && canonical == publicapi.CanonicalRepo(b.ValueString())
}

type poolRepoURLRequiresReplace struct{}

func (poolRepoURLRequiresReplace) Description(context.Context) string {
	return "If the value of this attribute changes, Terraform will destroy and recreate the resource, unless the new value is an equivalent spelling of the same repository."
}

func (m poolRepoURLRequiresReplace) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (poolRepoURLRequiresReplace) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || req.PlanValue.Equal(req.StateValue) {
		return
	}
	resp.RequiresReplace = !poolRepoURLEquivalent(req.StateValue, req.PlanValue)
}

func requirePools(client *apiClient) (*publicapi.Client, error) {
	if client == nil || client.pools == nil {
		return nil, fmt.Errorf("pool_api_key is required; set the provider pool_api_key attribute or %s", envPoolAPIKey)
	}
	return client.pools, nil
}

func poolRequest(model privateWorkerPoolModel) publicapi.RegisterPoolRequest {
	req := publicapi.RegisterPoolRequest{
		Scope:                     model.Scope.ValueString(),
		PoolName:                  model.PoolName.ValueString(),
		WorkerReadyTimeoutSeconds: model.WorkerReadyTimeoutSeconds.ValueInt64(),
	}
	if owner := optionalString(model.RepoOwner); owner != "" {
		req.RepoOwner = owner
		req.RepoName = optionalString(model.RepoName)
	}
	if raw := optionalString(model.RepoURL); raw != "" {
		req.RepoURL = raw
	}
	return req
}

// The id is split on "/", so a "/" inside a name is percent-encoded. Names without
// "/" or "%" are written unchanged.
var (
	poolIDEscaper   = strings.NewReplacer("%", "%25", "/", "%2F")
	poolIDUnescaper = strings.NewReplacer("%25", "%", "%2F", "/", "%2f", "/")
)

func poolTerraformID(model privateWorkerPoolModel) string {
	id := model.Scope.ValueString() + "/" + poolIDEscaper.Replace(model.PoolName.ValueString())
	if owner := optionalString(model.RepoOwner); owner != "" {
		id += "/" + poolIDEscaper.Replace(owner) + "/" + poolIDEscaper.Replace(optionalString(model.RepoName))
	}
	return id
}

func poolListScope(scope string) string {
	switch scope {
	case "team":
		return "team_pool"
	case "user":
		return "personal"
	default:
		return scope
	}
}

func parsePoolID(id string) (scope, name, owner, repo string, err error) {
	parts := strings.Split(id, "/")
	invalid := fmt.Errorf("expected <scope>/<pool_name> or <scope>/<pool_name>/<repo_owner>/<repo_name> with scope team or user, got %q; inside a name, write / as %%2F and %% as %%25", id)
	if len(parts) != 2 && len(parts) != 4 || parts[0] != "team" && parts[0] != "user" {
		return "", "", "", "", invalid
	}
	for i, part := range parts {
		if part == "" {
			return "", "", "", "", invalid
		}
		parts[i] = poolIDUnescaper.Replace(part)
	}
	if len(parts) == 2 {
		return parts[0], parts[1], "", "", nil
	}
	return parts[0], parts[1], parts[2], parts[3], nil
}

func optionalString(value types.String) string {
	if value.IsNull() || value.IsUnknown() {
		return ""
	}
	return value.ValueString()
}
