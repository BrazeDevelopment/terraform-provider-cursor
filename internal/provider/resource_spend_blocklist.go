package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type userSpendLimitResource struct{ client *apiClient }

type userSpendLimitModel struct {
	ID                types.String `tfsdk:"id"`
	UserEmail         types.String `tfsdk:"user_email"`
	SpendLimitDollars types.Int64  `tfsdk:"spend_limit_dollars"`
}

func NewUserSpendLimitResource() resource.Resource { return &userSpendLimitResource{} }

func (r *userSpendLimitResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_spend_limit"
}

func (r *userSpendLimitResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Sets one team member's monthly spend limit. spend_limit_dollars is an integer. 0 sets a zero-dollar limit. Null, including destroy, sends null and removes the limit. There is no dedicated read; refresh uses monthlyLimitDollars from the paginated spend list and ignores hardLimitOverrideDollars. Enterprise only. The bulk route is not used.",
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "The member email."},
			"user_email": schema.StringAttribute{Required: true, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, Description: "Email of the team member."},
			"spend_limit_dollars": schema.Int64Attribute{
				Optional:    true,
				Description: "Integer spend limit in dollars. 0 is a zero-dollar limit. Null removes the limit.",
			},
		},
	}
}

func (r *userSpendLimitResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client, resp.Diagnostics = configureClient(req, resp.Diagnostics)
}

func (r *userSpendLimitResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userSpendLimitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.setUserSpendLimit(ctx, plan.UserEmail.ValueString(), int64Ptr(plan.SpendLimitDollars)); err != nil {
		resp.Diagnostics.AddError("Failed to set user spend limit", err.Error())
		return
	}
	state, err := r.read(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read user spend limit", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userSpendLimitResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userSpendLimitModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	next, err := r.read(ctx, state)
	if err != nil {
		if isAdminNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read user spend limit", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

func (r *userSpendLimitResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan userSpendLimitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.setUserSpendLimit(ctx, plan.UserEmail.ValueString(), int64Ptr(plan.SpendLimitDollars)); err != nil {
		resp.Diagnostics.AddError("Failed to set user spend limit", err.Error())
		return
	}
	state, err := r.read(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read user spend limit", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userSpendLimitResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userSpendLimitModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.setUserSpendLimit(ctx, state.UserEmail.ValueString(), nil); err != nil && !isAdminNotFound(err) {
		resp.Diagnostics.AddError("Failed to clear user spend limit", err.Error())
	}
}

func (r *userSpendLimitResource) read(ctx context.Context, model userSpendLimitModel) (userSpendLimitModel, error) {
	limit, ok, err := r.client.findUserSpend(ctx, model.UserEmail.ValueString())
	if err != nil {
		return model, err
	}
	if !ok {
		return model, &adminStatusError{API: "Team Admin API", Status: http.StatusNotFound, Message: "user was not found in spend data"}
	}
	model.ID = model.UserEmail
	if limit == nil {
		model.SpendLimitDollars = types.Int64Null()
	} else {
		model.SpendLimitDollars = types.Int64Value(*limit)
	}
	return model, nil
}

func int64Ptr(value types.Int64) *int64 {
	if value.IsNull() || value.IsUnknown() {
		return nil
	}
	n := value.ValueInt64()
	return &n
}

type repoBlocklistResource struct{ client *apiClient }

type repoBlocklistModel struct {
	ID       types.String `tfsdk:"id"`
	URL      types.String `tfsdk:"url"`
	Patterns types.List   `tfsdk:"patterns"`
}

func NewRepoBlocklistResource() resource.Resource { return &repoBlocklistResource{} }

func (r *repoBlocklistResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_repo_blocklist"
}

func (r *repoBlocklistResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the blocklist patterns for one repository URL. Upsert replaces patterns for that repository and leaves every other repository alone. Delete uses the repo_ id from the read. Blocklists apply to the editor and CLI, not to Cloud Agents or automations.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "Repository blocklist id (repo_…)."},
			"url": schema.StringAttribute{
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Description:   "Repository URL.",
			},
			"patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Required:    true,
				Description: "File patterns to block. * blocks the entire repository.",
			},
		},
	}
}

func (r *repoBlocklistResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client, resp.Diagnostics = configureClient(req, resp.Diagnostics)
}

func (r *repoBlocklistResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan repoBlocklistModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	record, err := r.client.upsertRepoBlocklist(ctx, plan.URL.ValueString(), stringValues(ctx, plan.Patterns))
	if err != nil {
		resp.Diagnostics.AddError("Failed to upsert repository blocklist", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, blocklistModel(record))...)
}

func (r *repoBlocklistResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state repoBlocklistModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	repos, err := r.client.listRepoBlocklists(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read repository blocklists", err.Error())
		return
	}
	for _, repo := range repos {
		if repo.ID == state.ID.ValueString() || (state.ID.ValueString() == "" && repo.URL == state.URL.ValueString()) {
			resp.Diagnostics.Append(resp.State.Set(ctx, blocklistModel(&repo))...)
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

func (r *repoBlocklistResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan repoBlocklistModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	record, err := r.client.upsertRepoBlocklist(ctx, plan.URL.ValueString(), stringValues(ctx, plan.Patterns))
	if err != nil {
		resp.Diagnostics.AddError("Failed to upsert repository blocklist", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, blocklistModel(record))...)
}

func (r *repoBlocklistResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state repoBlocklistModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.deleteRepoBlocklist(ctx, state.ID.ValueString()); err != nil && !isAdminNotFound(err) {
		resp.Diagnostics.AddError("Failed to delete repository blocklist", err.Error())
	}
}

func blocklistModel(record *repoBlocklistRecord) *repoBlocklistModel {
	patterns := record.Patterns
	if patterns == nil {
		patterns = []string{}
	}
	list, _ := types.ListValueFrom(context.Background(), types.StringType, patterns)
	return &repoBlocklistModel{ID: types.StringValue(record.ID), URL: types.StringValue(record.URL), Patterns: list}
}
