package provider

import (
	"context"
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
	client *apiClient
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
		Description: "Registers a durable private worker pool. Register returns no id; the Terraform id is team/<pool_name> or team/<pool_name>/<repo_owner>/<repo_name> (user/... when scope is user). There is no update endpoint, so every argument change replaces the pool and removes it from pickers until the new one is registered. That includes worker_ready_timeout_seconds. Deregister is a soft-delete: the pool leaves pickers, and workers already connected stay connected. Authentication uses pool_api_key, one team service-account key for every pool in the team. Automations select the pool with private_worker.labels.pool. The repo label is reserved. This is a v0 API.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
				Description: "team/<pool_name>, team/<pool_name>/<repo_owner>/<repo_name>, or the same forms with user.",
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
				Description:   "Repository URL for display. Requires repo_owner and repo_name.",
				PlanModifiers: replace,
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
	ownerSet := !cfg.RepoOwner.IsNull() && !cfg.RepoOwner.IsUnknown() && cfg.RepoOwner.ValueString() != ""
	nameSet := !cfg.RepoName.IsNull() && !cfg.RepoName.IsUnknown() && cfg.RepoName.ValueString() != ""
	urlSet := !cfg.RepoURL.IsNull() && !cfg.RepoURL.IsUnknown() && cfg.RepoURL.ValueString() != ""
	if ownerSet != nameSet {
		resp.Diagnostics.AddError("Invalid worker pool", "repo_owner and repo_name must be set together.")
	}
	if urlSet && (!ownerSet || !nameSet) {
		resp.Diagnostics.AddError("Invalid worker pool", "repo_url requires repo_owner and repo_name.")
	}
	if !cfg.WorkerReadyTimeoutSeconds.IsUnknown() && !cfg.WorkerReadyTimeoutSeconds.IsNull() && cfg.WorkerReadyTimeoutSeconds.ValueInt64() < 0 {
		resp.Diagnostics.AddAttributeError(path.Root("worker_ready_timeout_seconds"), "Invalid worker pool", "worker_ready_timeout_seconds must be zero or greater.")
	}
}

func (r *privateWorkerPoolResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || r.client == nil || r.client.guardrails == nil || !r.client.guardrails.HasAllowedOwners {
		return
	}
	var plan privateWorkerPoolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() || plan.RepoOwner.IsUnknown() || plan.RepoOwner.IsNull() || plan.RepoOwner.ValueString() == "" {
		return
	}
	resp.Diagnostics.Append(r.client.guardrails.checkOwners("worker pool repository", []string{plan.RepoOwner.ValueString()})...)
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
	body := poolRequest(plan)
	if err := client.RegisterPool(ctx, body); err != nil {
		resp.Diagnostics.AddError("Failed to register private worker pool", err.Error())
		return
	}
	plan.ID = types.StringValue(poolTerraformID(plan))
	state, err := r.readPool(ctx, client, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read private worker pool", err.Error())
		return
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
	next, err := r.readPool(ctx, client, state)
	if err != nil {
		if publicapi.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read private worker pool", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

func (r *privateWorkerPoolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Private worker pools cannot be updated", "Every argument change forces replacement. There is no update endpoint.")
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
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("scope"), scope)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("pool_name"), name)...)
	if owner != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("repo_owner"), owner)...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("repo_name"), repo)...)
	}
}

func (r *privateWorkerPoolResource) readPool(ctx context.Context, client *publicapi.Client, model privateWorkerPoolModel) (privateWorkerPoolModel, error) {
	pools, err := client.ListPools(ctx, poolListScope(model.Scope.ValueString()))
	if err != nil {
		return model, err
	}
	wantOwner := optionalString(model.RepoOwner)
	wantRepo := optionalString(model.RepoName)
	for _, pool := range pools {
		if pool.Scope == model.Scope.ValueString() && pool.PoolName == model.PoolName.ValueString() && pool.RepoOwner == wantOwner && pool.RepoName == wantRepo {
			model.ID = types.StringValue(poolTerraformID(model))
			model.ConnectedWorkerCount = types.Int64Value(pool.ConnectedWorkerCount)
			model.InUseWorkerCount = types.Int64Value(pool.InUseWorkerCount)
			model.WorkerReadyTimeoutSeconds = types.Int64Value(pool.WorkerReadyTimeoutSeconds)
			model.RepoURL = keepOptionalRepoURL(model.RepoURL, pool.RepoURL)
			return model, nil
		}
	}
	return model, &publicapi.Error{Status: 404, Code: "pool_not_found", Message: "pool is not registered"}
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

func poolTerraformID(model privateWorkerPoolModel) string {
	id := model.Scope.ValueString() + "/" + model.PoolName.ValueString()
	if owner := optionalString(model.RepoOwner); owner != "" {
		id += "/" + owner + "/" + optionalString(model.RepoName)
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
	switch len(parts) {
	case 2:
		if parts[0] != "team" && parts[0] != "user" || parts[1] == "" {
			return "", "", "", "", fmt.Errorf("expected team/<pool_name> or user/<pool_name>, got %q", id)
		}
		return parts[0], parts[1], "", "", nil
	case 4:
		if parts[0] != "team" && parts[0] != "user" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
			return "", "", "", "", fmt.Errorf("expected <scope>/<pool_name>/<repo_owner>/<repo_name>, got %q", id)
		}
		return parts[0], parts[1], parts[2], parts[3], nil
	default:
		return "", "", "", "", fmt.Errorf("expected team/<pool_name> or team/<pool_name>/<repo_owner>/<repo_name>, got %q", id)
	}
}

func optionalString(value types.String) string {
	if value.IsNull() || value.IsUnknown() {
		return ""
	}
	return value.ValueString()
}
