package provider

import (
	"context"
	"fmt"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*environmentResource)(nil)
	_ resource.ResourceWithImportState = (*environmentResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*environmentResource)(nil)
)

type environmentResource struct {
	client *apiClient
}

type environmentRepoModel struct {
	URL types.String `tfsdk:"url"`
}

type environmentModel struct {
	ID              types.String           `tfsdk:"id"`
	Owner           types.String           `tfsdk:"owner"`
	Name            types.String           `tfsdk:"name"`
	Repos           []environmentRepoModel `tfsdk:"repos"`
	EnvironmentJSON types.String           `tfsdk:"environment_json"`
}

func NewEnvironmentResource() resource.Resource {
	return &environmentResource{}
}

func (r *environmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment"
}

func (r *environmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Cursor Cloud Agent environment. owner and repos force replacement because the API accepts only name and environmentJson on update, and an owner cannot have two environments with the same name. Replacing an environment changes its id, so automations that reference it must move in the same apply. A response that includes repoFile is configured from a file in a repository; that file is managed in git and the provider will not update the environment.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Environment UUID. Automations store this value in environment_public_id.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"owner": schema.StringAttribute{
				Required:    true,
				Description: "team or personal. Changing owner replaces the environment.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{stringvalidator.OneOf("team", "personal")},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Display name. It must be unique for the owner.",
			},
			"environment_json": schema.StringAttribute{
				Required:    true,
				Description: "environment.json as a JSON string, using the .cursor/environment.json schema. Key order and whitespace are normalized.",
				PlanModifiers: []planmodifier.String{
					normalizeEnvironmentJSONModifier{},
				},
			},
			"repos": schema.ListNestedAttribute{
				Required:    true,
				Description: "Repositories in the environment. An empty list creates an environment without repositories. Changing the list replaces the environment.",
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplace(),
				},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"url": schema.StringAttribute{
							Required:    true,
							Description: "Repository URL.",
						},
					},
				},
			},
		},
	}
}

type normalizeEnvironmentJSONModifier struct{}

func (m normalizeEnvironmentJSONModifier) Description(context.Context) string {
	return "Normalizes environment_json so key order and whitespace do not drift."
}

func (m normalizeEnvironmentJSONModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (m normalizeEnvironmentJSONModifier) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	normalized, err := normalizeEnvironmentJSON(req.ConfigValue.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid environment_json", err.Error())
		return
	}
	resp.PlanValue = types.StringValue(normalized)
}

func (r *environmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *environmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan environmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePublic(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	created, err := client.CreateEnvironment(ctx, publicapi.CreateEnvironmentRequest{
		Owner:           plan.Owner.ValueString(),
		Name:            plan.Name.ValueString(),
		Repos:           repoWrites(plan.Repos),
		EnvironmentJSON: plan.EnvironmentJSON.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create Cursor environment", err.Error())
		return
	}
	state, diags := environmentFromAPI(ctx, created, plan.EnvironmentJSON.ValueString())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	fresh, err := client.GetEnvironment(ctx, created.ID)
	if err == nil {
		state, diags = environmentFromAPI(ctx, fresh, plan.EnvironmentJSON.ValueString())
		resp.Diagnostics.Append(diags...)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *environmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state environmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePublic(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	env, err := client.GetEnvironment(ctx, state.ID.ValueString())
	if publicapi.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read Cursor environment", err.Error())
		return
	}
	if env.RepoFile != nil {
		resp.Diagnostics.AddWarning("Environment configuration is managed in git", fmt.Sprintf("This environment loads its configuration from %s in %s. That file is managed in git. The provider will not update it.", env.RepoFile.Path, env.RepoFile.URL))
	}
	next, diags := environmentFromAPI(ctx, env, state.EnvironmentJSON.ValueString())
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

func (r *environmentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior environmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePublic(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	current, err := client.GetEnvironment(ctx, prior.ID.ValueString())
	if publicapi.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read Cursor environment", err.Error())
		return
	}
	if current.RepoFile != nil {
		resp.Diagnostics.AddError("Environment configuration is managed in git", fmt.Sprintf("This environment loads its configuration from %s in %s. That file is managed in git. The provider will not PATCH it.", current.RepoFile.Path, current.RepoFile.URL))
		return
	}
	patch := publicapi.UpdateEnvironmentRequest{}
	if !plan.Name.Equal(prior.Name) {
		name := plan.Name.ValueString()
		patch.Name = &name
	}
	if !plan.EnvironmentJSON.Equal(prior.EnvironmentJSON) {
		raw := plan.EnvironmentJSON.ValueString()
		patch.EnvironmentJSON = &raw
	}
	if patch.Name != nil || patch.EnvironmentJSON != nil {
		if err := client.UpdateEnvironment(ctx, prior.ID.ValueString(), patch); err != nil {
			resp.Diagnostics.AddError("Failed to update Cursor environment", err.Error())
			return
		}
	}
	fresh, err := client.GetEnvironment(ctx, prior.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read Cursor environment", err.Error())
		return
	}
	state, diags := environmentFromAPI(ctx, fresh, plan.EnvironmentJSON.ValueString())
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *environmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state environmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePublic(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	if err := client.DeleteEnvironment(ctx, state.ID.ValueString()); err != nil && !publicapi.IsNotFound(err) {
		resp.Diagnostics.AddError("Failed to delete Cursor environment", err.Error())
	}
}

func (r *environmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Import an environment by its UUID.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

func (r *environmentResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || r.client == nil || r.client.guardrails == nil || !r.client.guardrails.HasAllowedOwners {
		return
	}
	var plan environmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var urls []string
	for _, repo := range plan.Repos {
		if repo.URL.IsUnknown() {
			return
		}
		if !repo.URL.IsNull() {
			urls = append(urls, repo.URL.ValueString())
		}
	}
	resp.Diagnostics.Append(r.client.guardrails.checkOwners("environment repository", urls)...)
}

func requirePublic(client *apiClient) (*publicapi.Client, error) {
	if client == nil || client.public == nil {
		return nil, fmt.Errorf("api_key is required; set the provider api_key attribute or %s", envAPIKey)
	}
	return client.public, nil
}

func repoWrites(repos []environmentRepoModel) []publicapi.EnvironmentRepo {
	out := make([]publicapi.EnvironmentRepo, 0, len(repos))
	for _, repo := range repos {
		out = append(out, publicapi.EnvironmentRepo{URL: repo.URL.ValueString()})
	}
	return out
}

func environmentFromAPI(_ context.Context, env *publicapi.Environment, fallbackJSON string) (environmentModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	repos := make([]environmentRepoModel, 0, len(env.Repos))
	for _, repo := range env.Repos {
		repos = append(repos, environmentRepoModel{URL: types.StringValue(repo.URL)})
	}
	raw := env.EnvironmentJSON
	if raw == "" {
		raw = fallbackJSON
	}
	if raw != "" {
		if normalized, err := normalizeEnvironmentJSON(raw); err == nil {
			raw = normalized
		}
	}
	jsonValue := types.StringNull()
	if raw != "" {
		jsonValue = types.StringValue(raw)
	}
	name := types.StringNull()
	if env.Name != "" {
		name = types.StringValue(env.Name)
	}
	return environmentModel{
		ID:              types.StringValue(env.ID),
		Owner:           types.StringValue(env.Owner),
		Name:            name,
		Repos:           repos,
		EnvironmentJSON: jsonValue,
	}, diags
}
