package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
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
	client   *apiClient
	provider *cursorProvider
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

const environmentReposReplaceDescription = "If the repositories change, Terraform will destroy and recreate the resource. A different spelling of the same repositories in the same order does not."

func (r *environmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Cursor Cloud Agent environment. owner and repos force replacement because the API accepts only name and environmentJson on update, and an owner cannot have two environments with the same name. Replacing an environment changes its id, so automations that reference it must move in the same apply. A response that includes repoFile is configured from a file in a repository; that file is managed in git and the provider will not update the environment. Import with the environment UUID.",
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
				Description: "Display name, 1 to 255 characters. It must be unique for the owner.",
				Validators:  []validator.String{stringvalidator.UTF8LengthBetween(1, 255)},
			},
			"environment_json": schema.StringAttribute{
				Required:    true,
				Description: "environment.json as a JSON string holding one JSON object, using the .cursor/environment.json schema. Refresh keeps the configured text while the saved configuration is the same JSON, so key order and whitespace do not cause drift.",
				Validators:  []validator.String{environmentJSONValidator{}},
			},
			"repos": schema.ListNestedAttribute{
				Required:    true,
				Description: "Repositories in the environment, at most 100, each listed once. An empty list creates an environment without repositories. Adding, removing, or reordering repositories replaces the environment. Respelling a URL that names the same repository, such as adding .git or changing letter case, does not.",
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplaceIf(environmentReposRequireReplace, environmentReposReplaceDescription, environmentReposReplaceDescription),
				},
				Validators: []validator.List{
					listvalidator.SizeAtMost(100),
					environmentReposValidator{},
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

type environmentJSONValidator struct{}

func (v environmentJSONValidator) Description(context.Context) string {
	return "Must be a single valid JSON object."
}

func (v environmentJSONValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v environmentJSONValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	normalized, err := normalizeEnvironmentJSON(req.ConfigValue.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid environment_json", err.Error())
		return
	}
	if !strings.HasPrefix(normalized, "{") {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid environment_json", "environment_json must be a JSON object, as in a .cursor/environment.json file.")
	}
}

// environmentReposValidator rejects a blank url and a repository listed twice.
// Unknown values are skipped; Terraform validates again once they are known.
type environmentReposValidator struct{}

func (v environmentReposValidator) Description(context.Context) string {
	return "Each url must be set, and a repository can appear only once."
}

func (v environmentReposValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v environmentReposValidator) ValidateList(_ context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	seen := map[string]int{}
	for i, element := range knownElements(req.ConfigValue) {
		url, ok := knownAttributes(element)["url"].(types.String)
		if !ok || url.IsNull() || url.IsUnknown() {
			continue
		}
		p := req.Path.AtListIndex(i).AtName("url")
		canonical := publicapi.CanonicalRepo(url.ValueString())
		if canonical == "" {
			resp.Diagnostics.AddAttributeError(p, "Invalid repository URL", "url must not be blank.")
			continue
		}
		if first, dup := seen[canonical]; dup {
			resp.Diagnostics.AddAttributeError(p, "Duplicate repository", fmt.Sprintf("%q names the same repository as repos[%d]. List each repository once.", url.ValueString(), first))
			continue
		}
		seen[canonical] = i
	}
}

// environmentRepoURLs reports false when the list or any url in it is null or unknown.
func environmentRepoURLs(list types.List) ([]string, bool) {
	if list.IsNull() || list.IsUnknown() {
		return nil, false
	}
	urls := make([]string, 0, len(list.Elements()))
	for _, element := range list.Elements() {
		url, ok := knownAttributes(element)["url"].(types.String)
		if !ok || url.IsNull() || url.IsUnknown() {
			return nil, false
		}
		urls = append(urls, url.ValueString())
	}
	return urls, true
}

// The comparison keeps order: whether the API gives meaning to the order of
// repositories is not documented, so a reorder still replaces the environment.
func environmentReposRequireReplace(_ context.Context, req planmodifier.ListRequest, resp *listplanmodifier.RequiresReplaceIfFuncResponse) {
	state, stateKnown := environmentRepoURLs(req.StateValue)
	plan, planKnown := environmentRepoURLs(req.PlanValue)
	if !stateKnown || !planKnown || len(state) != len(plan) {
		resp.RequiresReplace = true
		return
	}
	for i := range plan {
		if publicapi.CanonicalRepo(plan[i]) != publicapi.CanonicalRepo(state[i]) {
			resp.RequiresReplace = true
			return
		}
	}
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
	if created.ID == "" {
		resp.Diagnostics.AddError("Failed to create Cursor environment", fmt.Sprintf("The API accepted the environment and did not return its id. The environment may exist. Find the id of the %s environment named %q with the cursor_environments data source and import it before applying again.", plan.Owner.ValueString(), plan.Name.ValueString()))
		return
	}
	// State keeps the planned values, which Terraform requires after apply. A value
	// the API stored differently shows up as drift on the next refresh.
	state := plan
	state.ID = types.StringValue(created.ID)
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
	next := environmentFromAPI(env, state)
	if env.ID == "" {
		next.ID = state.ID
	}
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
	id := prior.ID.ValueString()
	current, err := client.GetEnvironment(ctx, id)
	if publicapi.IsNotFound(err) {
		// Terraform rejects an update that returns no state, so the deletion is
		// reported here and the next refresh removes the resource.
		resp.Diagnostics.AddError("Cursor environment no longer exists", fmt.Sprintf("Environment %s was deleted outside Terraform. Run terraform apply again to create it.", id))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read Cursor environment", err.Error())
		return
	}
	// The API cannot change repositories, and the plan decided against replacement
	// by comparing with state. State can be stale when refresh was skipped.
	if !slices.Equal(canonicalRepoMultiset(environmentModelRepoURLs(plan.Repos)), canonicalRepoMultiset(environmentAPIRepoURLs(current.Repos))) {
		resp.Diagnostics.AddAttributeError(path.Root("repos"), "Environment repositories changed outside Terraform", "The environment's repositories no longer match this plan, and the API cannot change them in place. Run terraform plan again; it will propose replacing the environment.")
		return
	}
	patch := publicapi.UpdateEnvironmentRequest{}
	if plan.Name.ValueString() != current.Name {
		name := plan.Name.ValueString()
		patch.Name = &name
	}
	if planned, err := normalizeEnvironmentJSON(plan.EnvironmentJSON.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("environment_json"), "Invalid environment_json", err.Error())
		return
	} else if saved, _ := normalizeEnvironmentJSON(current.EnvironmentJSON); planned != saved {
		patch.EnvironmentJSON = &planned
	}
	if patch.Name != nil || patch.EnvironmentJSON != nil {
		if current.RepoFile != nil {
			resp.Diagnostics.AddError("Environment configuration is managed in git", fmt.Sprintf("This environment loads its configuration from %s in %s. That file is managed in git. The provider will not PATCH it.", current.RepoFile.Path, current.RepoFile.URL))
			return
		}
		if err := client.UpdateEnvironment(ctx, id, patch); err != nil {
			resp.Diagnostics.AddError("Failed to update Cursor environment", err.Error())
			return
		}
	}
	// The PATCH returns no body. State keeps the planned values rather than a read
	// that may not reflect the write yet; a value the API stored differently shows
	// up as drift on the next refresh.
	state := plan
	state.ID = prior.ID
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
	id := strings.TrimSpace(req.ID)
	if id == "" || strings.ContainsAny(id, "/ \t") {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Import an environment by its UUID, for example 8f14e45f-ceea-4e6b-9c3a-1d2e3f4a5b6c. Got %q.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}

func (r *environmentResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	guardrails := planGuardrails(r.client, r.provider)
	if req.Plan.Raw.IsNull() || guardrails == nil || !guardrails.HasAllowedOwners {
		return
	}
	// repos is read as a framework value: the model's slice cannot hold an unknown list.
	var repos types.List
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("repos"), &repos)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(checkEnvironmentOwnerGuardrail(guardrails, repos)...)
}

// Known urls are checked even when other urls are still unknown. Unknown urls are
// checked when Terraform plans again during apply, where everything is known.
func checkEnvironmentOwnerGuardrail(g *guardrailsConfig, repos types.List) diag.Diagnostics {
	var diags diag.Diagnostics
	for i, element := range knownElements(repos) {
		url, ok := knownAttributes(element)["url"].(types.String)
		if !ok || url.IsNull() || url.IsUnknown() {
			continue
		}
		if detail := g.ownerViolation("environment repository", url.ValueString()); detail != "" {
			diags.AddAttributeError(path.Root("repos").AtListIndex(i).AtName("url"), "Repository owner is not allowed", detail)
		}
	}
	return diags
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

func environmentModelRepoURLs(repos []environmentRepoModel) []string {
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		out = append(out, repo.URL.ValueString())
	}
	return out
}

func environmentAPIRepoURLs(repos []publicapi.EnvironmentRepo) []string {
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		out = append(out, repo.URL)
	}
	return out
}

func environmentFromAPI(env *publicapi.Environment, prior environmentModel) environmentModel {
	name := types.StringNull()
	if env.Name != "" {
		name = types.StringValue(env.Name)
	}
	return environmentModel{
		ID:              types.StringValue(env.ID),
		Owner:           types.StringValue(env.Owner),
		Name:            name,
		Repos:           keepEquivalentEnvironmentRepos(prior.Repos, env.Repos),
		EnvironmentJSON: keepEquivalentJSON(prior.EnvironmentJSON, env.EnvironmentJSON),
	}
}
