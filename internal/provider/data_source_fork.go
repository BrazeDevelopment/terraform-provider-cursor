package provider

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var environmentIDValidators = []validator.String{
	stringvalidator.RegexMatches(regexp.MustCompile(`^\S+$`), "must be an environment id with no whitespace"),
}

type apiKeyInfoDataSource struct{ client *apiClient }

type apiKeyInfoModel struct {
	ID            types.String `tfsdk:"id"`
	APIKeyName    types.String `tfsdk:"api_key_name"`
	CreatedAt     types.String `tfsdk:"created_at"`
	UserID        types.String `tfsdk:"user_id"`
	UserEmail     types.String `tfsdk:"user_email"`
	UserFirstName types.String `tfsdk:"user_first_name"`
	UserLastName  types.String `tfsdk:"user_last_name"`
}

func NewAPIKeyInfoDataSource() datasource.DataSource { return &apiKeyInfoDataSource{} }

func (d *apiKeyInfoDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_api_key_info"
}

func (d *apiKeyInfoDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads GET /v1/me for the provider api_key. api_key_name and created_at are always set. User attributes are set for a user-scoped key and null for a service account. A service account returns no team id, so a precondition can compare api_key_name and cannot prove which team the key belongs to.",
		Attributes: map[string]schema.Attribute{
			"id":              schema.StringAttribute{Computed: true, Description: "API key name."},
			"api_key_name":    schema.StringAttribute{Computed: true, Description: "Display name of the API key."},
			"created_at":      schema.StringAttribute{Computed: true, Description: "When the API key was created."},
			"user_id":         schema.StringAttribute{Computed: true, Description: "Numeric user id. Null for a service account."},
			"user_email":      schema.StringAttribute{Computed: true, Description: "User email. Null for a service account."},
			"user_first_name": schema.StringAttribute{Computed: true, Description: "User first name. Null for a service account."},
			"user_last_name":  schema.StringAttribute{Computed: true, Description: "User last name. Null for a service account."},
		},
	}
}

func (d *apiKeyInfoDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client, resp.Diagnostics = configureDataClient(req, resp.Diagnostics)
}

func (d *apiKeyInfoDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, err := requirePublic(d.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	info, err := client.APIKeyInfo(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read API key info", err.Error())
		return
	}
	model := apiKeyInfoModel{
		ID:            types.StringValue(info.APIKeyName),
		APIKeyName:    types.StringValue(info.APIKeyName),
		CreatedAt:     types.StringValue(info.CreatedAt),
		UserID:        types.StringNull(),
		UserEmail:     stringOrNull(info.UserEmail),
		UserFirstName: stringOrNull(info.UserFirstName),
		UserLastName:  stringOrNull(info.UserLastName),
	}
	if info.UserID != nil {
		model.UserID = types.StringValue(strconv.FormatInt(*info.UserID, 10))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

type teamMembersDataSource struct{ client *apiClient }

type teamMemberModel struct {
	ID        types.String `tfsdk:"id"`
	Email     types.String `tfsdk:"email"`
	Name      types.String `tfsdk:"name"`
	Role      types.String `tfsdk:"role"`
	IsRemoved types.Bool   `tfsdk:"is_removed"`
}

type teamMembersModel struct {
	ID      types.String      `tfsdk:"id"`
	Members []teamMemberModel `tfsdk:"members"`
}

func NewTeamMembersDataSource() datasource.DataSource { return &teamMembersDataSource{} }

func (d *teamMembersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_team_members"
}

func (d *teamMembersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads Admin API team members with the provider team_api_key. role is member or owner. There is no admin value. Use a lifecycle precondition, not a check block, to fail the plan when membership does not match the approved list.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "team."},
			"members": schema.ListNestedAttribute{
				Computed:    true,
				Description: "Team members, including removed members, sorted by email and then id. Empty when the team has no members.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":         schema.StringAttribute{Computed: true, Description: "Member id."},
					"email":      schema.StringAttribute{Computed: true, Description: "Member email."},
					"name":       schema.StringAttribute{Computed: true, Description: "Member display name."},
					"role":       schema.StringAttribute{Computed: true, Description: "member or owner."},
					"is_removed": schema.BoolAttribute{Computed: true, Description: "True for a member who was removed from the team. Removed members stay in the list."},
				}},
			},
		},
	}
}

func (d *teamMembersDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client, resp.Diagnostics = configureDataClient(req, resp.Diagnostics)
}

func (d *teamMembersDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		resp.Diagnostics.AddError("Provider not configured", fmt.Sprintf("team_api_key is required; set the provider team_api_key attribute or %s", envTeamAPIKey))
		return
	}
	members, err := d.client.listTeamMemberRecords(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read team members", err.Error())
		return
	}
	model := teamMembersModel{ID: types.StringValue("team"), Members: make([]teamMemberModel, 0, len(members))}
	for _, member := range members {
		model.Members = append(model.Members, teamMemberModel{
			ID:        types.StringValue(member.ID),
			Email:     types.StringValue(member.Email),
			Name:      types.StringValue(member.Name),
			Role:      types.StringValue(member.Role),
			IsRemoved: types.BoolValue(member.IsRemoved),
		})
	}
	slices.SortStableFunc(model.Members, func(a, b teamMemberModel) int {
		return cmp.Or(
			cmp.Compare(strings.ToLower(a.Email.ValueString()), strings.ToLower(b.Email.ValueString())),
			cmp.Compare(a.ID.ValueString(), b.ID.ValueString()),
		)
	})
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

type environmentDataSource struct{ client *apiClient }

type environmentDataModel struct {
	ID              types.String           `tfsdk:"id"`
	EnvironmentID   types.String           `tfsdk:"environment_id"`
	Name            types.String           `tfsdk:"name"`
	Owner           types.String           `tfsdk:"owner"`
	Repos           []environmentRepoModel `tfsdk:"repos"`
	EnvironmentJSON types.String           `tfsdk:"environment_json"`
	VersionID       types.String           `tfsdk:"version_id"`
	RepoFileURL     types.String           `tfsdk:"repo_file_url"`
	RepoFilePath    types.String           `tfsdk:"repo_file_path"`
	CreatedAt       types.String           `tfsdk:"created_at"`
	UpdatedAt       types.String           `tfsdk:"updated_at"`
}

func NewEnvironmentDataSource() datasource.DataSource { return &environmentDataSource{} }

func (d *environmentDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment"
}

func (d *environmentDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads one Cloud Agent environment, including environmentJson and versionId. List items omit those fields. An environment that does not exist, or that the provider api_key cannot see, fails the read.",
		Attributes: map[string]schema.Attribute{
			"id":               schema.StringAttribute{Computed: true, Description: "Environment UUID. Same as environment_id."},
			"environment_id":   schema.StringAttribute{Required: true, Description: "Environment UUID.", Validators: environmentIDValidators},
			"name":             schema.StringAttribute{Computed: true, Description: "Display name. Null when the environment has no name."},
			"owner":            schema.StringAttribute{Computed: true, Description: "team or personal."},
			"environment_json": schema.StringAttribute{Computed: true, Description: "The latest saved environment.json as a JSON string. Null when the API returns none."},
			"version_id":       schema.StringAttribute{Computed: true, Description: "Id of the latest saved environment version. Null when no version has been saved."},
			"repo_file_url":    schema.StringAttribute{Computed: true, Description: "Repository that holds the configuration file. Set when the environment loads its configuration from a file in a repository, null otherwise."},
			"repo_file_path":   schema.StringAttribute{Computed: true, Description: "Path of the configuration file in that repository. Null unless repo_file_url is set."},
			"created_at":       schema.StringAttribute{Computed: true, Description: "When the environment was created."},
			"updated_at":       schema.StringAttribute{Computed: true, Description: "When the environment was last updated."},
			"repos": schema.ListNestedAttribute{
				Computed:    true,
				Description: "Repositories in the environment, in the order the API returns them. Empty for an environment without repositories.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"url": schema.StringAttribute{Computed: true, Description: "Repository URL."},
				}},
			},
		},
	}
}

func (d *environmentDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client, resp.Diagnostics = configureDataClient(req, resp.Diagnostics)
}

func (d *environmentDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg environmentDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePublic(d.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	id := cfg.EnvironmentID.ValueString()
	env, err := client.GetEnvironment(ctx, id)
	if err != nil {
		addEnvironmentReadError(&resp.Diagnostics, "Failed to read Cursor environment", id, err)
		return
	}
	if env.ID == "" {
		resp.Diagnostics.AddError("Failed to read Cursor environment", fmt.Sprintf("The API response for environment %q did not include an id.", id))
		return
	}
	model := environmentDataFromAPI(env)
	model.EnvironmentID = cfg.EnvironmentID
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// addEnvironmentReadError names the environment when the API answers 404, which it
// does both for an id that does not exist and for one the key cannot see.
func addEnvironmentReadError(diags *diag.Diagnostics, summary, id string, err error) {
	if publicapi.HasCode(err, "build_not_found") || !publicapi.IsNotFound(err) {
		diags.AddError(summary, err.Error())
		return
	}
	diags.AddAttributeError(path.Root("environment_id"), "Cursor environment not found", fmt.Sprintf("No environment with id %q is visible to the provider api_key. Either it does not exist or the key cannot access it. %s", id, err))
}

func environmentReposFromAPI(repos []publicapi.EnvironmentRepo) []environmentRepoModel {
	out := make([]environmentRepoModel, 0, len(repos))
	for _, repo := range repos {
		out = append(out, environmentRepoModel{URL: types.StringValue(repo.URL)})
	}
	return out
}

func environmentDataFromAPI(env *publicapi.Environment) environmentDataModel {
	model := environmentDataModel{
		ID:              types.StringValue(env.ID),
		EnvironmentID:   types.StringValue(env.ID),
		Name:            stringOrNull(env.Name),
		Owner:           types.StringValue(env.Owner),
		Repos:           environmentReposFromAPI(env.Repos),
		EnvironmentJSON: stringOrNull(env.EnvironmentJSON),
		VersionID:       stringOrNull(env.VersionID),
		RepoFileURL:     types.StringNull(),
		RepoFilePath:    types.StringNull(),
		CreatedAt:       types.StringValue(env.CreatedAt),
		UpdatedAt:       types.StringValue(env.UpdatedAt),
	}
	if env.RepoFile != nil {
		model.RepoFileURL = types.StringValue(env.RepoFile.URL)
		model.RepoFilePath = types.StringValue(env.RepoFile.Path)
	}
	return model
}

type environmentsDataSource struct{ client *apiClient }

type environmentListItemModel struct {
	ID           types.String           `tfsdk:"id"`
	Name         types.String           `tfsdk:"name"`
	Owner        types.String           `tfsdk:"owner"`
	Repos        []environmentRepoModel `tfsdk:"repos"`
	RepoFileURL  types.String           `tfsdk:"repo_file_url"`
	RepoFilePath types.String           `tfsdk:"repo_file_path"`
	CreatedAt    types.String           `tfsdk:"created_at"`
	UpdatedAt    types.String           `tfsdk:"updated_at"`
}

type environmentsModel struct {
	ID           types.String               `tfsdk:"id"`
	Environments []environmentListItemModel `tfsdk:"environments"`
}

func NewEnvironmentsDataSource() datasource.DataSource { return &environmentsDataSource{} }

func (d *environmentsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environments"
}

func (d *environmentsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Lists the Cloud Agent environments the provider api_key can access, following nextCursor until it is absent. A page can be shorter than the limit. An environment updated during the walk moves to the front, so a later page can skip it or repeat another; a repeated environment is returned once, and the data source returns the walk it completed. For a key that is neither a team admin nor a service account, the API can also leave out team environments it has not finished checking. List items omit environmentJson and versionId.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "environments."},
			"environments": schema.ListNestedAttribute{
				Computed:    true,
				Description: "Environments, sorted by id so that an update to one environment does not reorder the list. Empty when the key can access none.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":             schema.StringAttribute{Computed: true, Description: "Environment UUID."},
					"name":           schema.StringAttribute{Computed: true, Description: "Display name. Null when the environment has no name."},
					"owner":          schema.StringAttribute{Computed: true, Description: "team or personal."},
					"repo_file_url":  schema.StringAttribute{Computed: true, Description: "Repository that holds the configuration file. Set when the environment loads its configuration from a file in a repository, null otherwise."},
					"repo_file_path": schema.StringAttribute{Computed: true, Description: "Path of the configuration file in that repository. Null unless repo_file_url is set."},
					"created_at":     schema.StringAttribute{Computed: true, Description: "When the environment was created."},
					"updated_at":     schema.StringAttribute{Computed: true, Description: "When the environment was last updated."},
					"repos": schema.ListNestedAttribute{
						Computed:    true,
						Description: "Repositories in the environment, in the order the API returns them. Empty for an environment without repositories.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"url": schema.StringAttribute{Computed: true, Description: "Repository URL."},
						}},
					},
				}},
			},
		},
	}
}

func (d *environmentsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client, resp.Diagnostics = configureDataClient(req, resp.Diagnostics)
}

func (d *environmentsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, err := requirePublic(d.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	envs, err := client.ListEnvironments(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to list Cursor environments", err.Error())
		return
	}
	model := environmentsModel{ID: types.StringValue("environments"), Environments: make([]environmentListItemModel, 0, len(envs))}
	for _, env := range envs {
		item := environmentListItemModel{
			ID:           types.StringValue(env.ID),
			Name:         stringOrNull(env.Name),
			Owner:        types.StringValue(env.Owner),
			Repos:        environmentReposFromAPI(env.Repos),
			RepoFileURL:  types.StringNull(),
			RepoFilePath: types.StringNull(),
			CreatedAt:    types.StringValue(env.CreatedAt),
			UpdatedAt:    types.StringValue(env.UpdatedAt),
		}
		if env.RepoFile != nil {
			item.RepoFileURL = types.StringValue(env.RepoFile.URL)
			item.RepoFilePath = types.StringValue(env.RepoFile.Path)
		}
		model.Environments = append(model.Environments, item)
	}
	// The API lists the most recently updated environment first, so its order
	// changes whenever any environment is edited.
	slices.SortStableFunc(model.Environments, func(a, b environmentListItemModel) int {
		return cmp.Compare(a.ID.ValueString(), b.ID.ValueString())
	})
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

type environmentActiveBuildDataSource struct{ client *apiClient }

type environmentActiveBuildModel struct {
	ID            types.String `tfsdk:"id"`
	EnvironmentID types.String `tfsdk:"environment_id"`
	Type          types.String `tfsdk:"type"`
	BuildID       types.String `tfsdk:"build_id"`
	Status        types.String `tfsdk:"status"`
}

func NewEnvironmentActiveBuildDataSource() datasource.DataSource {
	return &environmentActiveBuildDataSource{}
}

func (d *environmentActiveBuildDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment_active_build"
}

func (d *environmentActiveBuildDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads what an agent boots from. type is build or universal_image. universal_image means Cursor's default image and has no status. For a build, status comes from GET /v1/environments/{id}/builds/{buildId} and is IN_PROGRESS, SUCCEEDED, FAILED, CANCELLED, or SKIPPED. A configuration change starts that build asynchronously, so a precondition on status cannot pass in the same apply that changed the environment. An environment that does not exist, or that the provider api_key cannot see, fails the read.",
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true, Description: "Environment UUID. Same as environment_id."},
			"environment_id": schema.StringAttribute{Required: true, Description: "Environment UUID.", Validators: environmentIDValidators},
			"type":           schema.StringAttribute{Computed: true, Description: "build or universal_image."},
			"build_id":       schema.StringAttribute{Computed: true, Description: "Id of the active build. Null when type is universal_image."},
			"status":         schema.StringAttribute{Computed: true, Description: "Status of the active build: IN_PROGRESS, SUCCEEDED, FAILED, CANCELLED, or SKIPPED. Null when type is universal_image."},
		},
	}
}

func (d *environmentActiveBuildDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client, resp.Diagnostics = configureDataClient(req, resp.Diagnostics)
}

func (d *environmentActiveBuildDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg environmentActiveBuildModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePublic(d.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	id := cfg.EnvironmentID.ValueString()
	active, err := client.GetActiveBuild(ctx, id)
	if err != nil {
		addEnvironmentReadError(&resp.Diagnostics, "Failed to read active environment build", id, err)
		return
	}
	if active.Type == "" {
		resp.Diagnostics.AddError("Failed to read active environment build", fmt.Sprintf("The API response for environment %q did not include a type.", id))
		return
	}
	model := environmentActiveBuildModel{
		ID:            types.StringValue(id),
		EnvironmentID: cfg.EnvironmentID,
		Type:          types.StringValue(active.Type),
		BuildID:       types.StringNull(),
		Status:        types.StringNull(),
	}
	if active.Type == "build" && active.BuildID != "" {
		model.BuildID = types.StringValue(active.BuildID)
		build, err := client.GetBuild(ctx, id, active.BuildID)
		if err != nil {
			addEnvironmentReadError(&resp.Diagnostics, "Failed to read environment build", id, err)
			return
		}
		model.Status = stringOrNull(build.Status)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

type modelsDataSource struct{ client *apiClient }

type modelParameterValueModel struct {
	Value       types.String `tfsdk:"value"`
	DisplayName types.String `tfsdk:"display_name"`
}

type modelParameterDataModel struct {
	ID          types.String               `tfsdk:"id"`
	DisplayName types.String               `tfsdk:"display_name"`
	Values      []modelParameterValueModel `tfsdk:"values"`
}

type modelVariantParamModel struct {
	ID    types.String `tfsdk:"id"`
	Value types.String `tfsdk:"value"`
}

type modelVariantModel struct {
	DisplayName types.String             `tfsdk:"display_name"`
	Description types.String             `tfsdk:"description"`
	IsDefault   types.Bool               `tfsdk:"is_default"`
	Params      []modelVariantParamModel `tfsdk:"params"`
}

type modelDataModel struct {
	ID          types.String              `tfsdk:"id"`
	DisplayName types.String              `tfsdk:"display_name"`
	Description types.String              `tfsdk:"description"`
	Aliases     types.List                `tfsdk:"aliases"`
	Parameters  []modelParameterDataModel `tfsdk:"parameters"`
	Variants    []modelVariantModel       `tfsdk:"variants"`
}

type modelsModel struct {
	ID     types.String     `tfsdk:"id"`
	Models []modelDataModel `tfsdk:"models"`
}

func NewModelsDataSource() datasource.DataSource { return &modelsDataSource{} }

func (d *modelsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_models"
}

func (d *modelsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads GET /v1/models, the catalog of model.id values accepted by POST /v1/agents, including parameters and variants. Do not use it to validate automation model or Admin model-access ids until Cursor confirms the catalogs are the same.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "models."},
			"models": schema.ListNestedAttribute{
				Computed:    true,
				Description: "Models, sorted by id. Lists inside a model keep the order the API returns and are empty, not null, when the API returns none.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":           schema.StringAttribute{Computed: true, Description: "Model id, the value POST /v1/agents accepts as model.id."},
					"display_name": schema.StringAttribute{Computed: true, Description: "Human-readable model name."},
					"description":  schema.StringAttribute{Computed: true, Description: "Model description. Null when the API returns none."},
					"aliases":      schema.ListAttribute{ElementType: types.StringType, Computed: true, Description: "Alternate ids that resolve to the same model."},
					"parameters": schema.ListNestedAttribute{
						Computed:    true,
						Description: "Parameters the model accepts.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"id":           schema.StringAttribute{Computed: true, Description: "Parameter id."},
							"display_name": schema.StringAttribute{Computed: true, Description: "Human-readable parameter name. Null when the API returns none."},
							"values": schema.ListNestedAttribute{
								Computed:    true,
								Description: "Values the parameter accepts.",
								NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
									"value":        schema.StringAttribute{Computed: true, Description: "Permitted value."},
									"display_name": schema.StringAttribute{Computed: true, Description: "Human-readable label for the value. Null when the API returns none."},
								}},
							},
						}},
					},
					"variants": schema.ListNestedAttribute{
						Computed:    true,
						Description: "Combinations of the model id and parameter values that the model accepts.",
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"display_name": schema.StringAttribute{Computed: true, Description: "Human-readable variant name."},
							"description":  schema.StringAttribute{Computed: true, Description: "Variant description. Null when the API returns none."},
							"is_default":   schema.BoolAttribute{Computed: true, Description: "True for the variant Cursor selects when the model is chosen without parameters."},
							"params": schema.ListNestedAttribute{
								Computed:    true,
								Description: "Parameter values of the variant. Can be empty.",
								NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
									"id":    schema.StringAttribute{Computed: true, Description: "Parameter id."},
									"value": schema.StringAttribute{Computed: true, Description: "Parameter value."},
								}},
							},
						}},
					},
				}},
			},
		},
	}
}

func (d *modelsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client, resp.Diagnostics = configureDataClient(req, resp.Diagnostics)
}

func (d *modelsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, err := requirePublic(d.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to list models", err.Error())
		return
	}
	state := modelsModel{ID: types.StringValue("models"), Models: make([]modelDataModel, 0, len(models))}
	for _, model := range models {
		aliases := model.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		aliasList, diags := types.ListValueFrom(ctx, types.StringType, aliases)
		resp.Diagnostics.Append(diags...)
		item := modelDataModel{
			ID:          types.StringValue(model.ID),
			DisplayName: types.StringValue(model.DisplayName),
			Description: stringOrNull(model.Description),
			Aliases:     aliasList,
			Parameters:  make([]modelParameterDataModel, 0, len(model.Parameters)),
			Variants:    make([]modelVariantModel, 0, len(model.Variants)),
		}
		for _, param := range model.Parameters {
			p := modelParameterDataModel{
				ID:          types.StringValue(param.ID),
				DisplayName: stringOrNull(param.DisplayName),
				Values:      make([]modelParameterValueModel, 0, len(param.Values)),
			}
			for _, value := range param.Values {
				p.Values = append(p.Values, modelParameterValueModel{Value: types.StringValue(value.Value), DisplayName: stringOrNull(value.DisplayName)})
			}
			item.Parameters = append(item.Parameters, p)
		}
		for _, variant := range model.Variants {
			v := modelVariantModel{
				DisplayName: types.StringValue(variant.DisplayName),
				Description: stringOrNull(variant.Description),
				IsDefault:   types.BoolValue(variant.IsDefault),
				Params:      make([]modelVariantParamModel, 0, len(variant.Params)),
			}
			for _, param := range variant.Params {
				v.Params = append(v.Params, modelVariantParamModel{ID: types.StringValue(param.ID), Value: types.StringValue(param.Value)})
			}
			item.Variants = append(item.Variants, v)
		}
		state.Models = append(state.Models, item)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	slices.SortStableFunc(state.Models, func(a, b modelDataModel) int {
		return cmp.Compare(a.ID.ValueString(), b.ID.ValueString())
	})
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

type privateWorkerPoolsDataSource struct{ client *apiClient }

type privateWorkerPoolDataModel struct {
	ID                        types.String `tfsdk:"id"`
	Scope                     types.String `tfsdk:"scope"`
	PoolName                  types.String `tfsdk:"pool_name"`
	RepoOwner                 types.String `tfsdk:"repo_owner"`
	RepoName                  types.String `tfsdk:"repo_name"`
	RepoURL                   types.String `tfsdk:"repo_url"`
	ConnectedWorkerCount      types.Int64  `tfsdk:"connected_worker_count"`
	InUseWorkerCount          types.Int64  `tfsdk:"in_use_worker_count"`
	WorkerReadyTimeoutSeconds types.Int64  `tfsdk:"worker_ready_timeout_seconds"`
	IsStale                   types.Bool   `tfsdk:"is_stale"`
}

type privateWorkerPoolsModel struct {
	ID    types.String                 `tfsdk:"id"`
	Scope types.String                 `tfsdk:"scope"`
	Pools []privateWorkerPoolDataModel `tfsdk:"pools"`
}

func NewPrivateWorkerPoolsDataSource() datasource.DataSource {
	return &privateWorkerPoolsDataSource{}
}

func (d *privateWorkerPoolsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_private_worker_pools"
}

func (d *privateWorkerPoolsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Lists private worker pools with the provider pool_api_key. The list filter is team_pool, personal, or all. cursor_private_worker_pool names the same scopes team and user; the data source accepts those too and sends team_pool and personal.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "The list filter that was sent: team_pool, personal, or all."},
			"scope": schema.StringAttribute{
				Optional:    true,
				Description: "team_pool, personal, or all. Defaults to all. team is accepted for team_pool and user for personal.",
				Validators:  []validator.String{stringvalidator.OneOf("team_pool", "personal", "all", "team", "user")},
			},
			"pools": schema.ListNestedAttribute{
				Computed:    true,
				Description: "Pools, sorted by id. Empty when no pool matches.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":                           schema.StringAttribute{Computed: true, Description: "The id cursor_private_worker_pool uses for this pool and accepts on import: <scope>/<pool_name>, or <scope>/<pool_name>/<repo_owner>/<repo_name> for a repo-backed pool."},
					"scope":                        schema.StringAttribute{Computed: true, Description: "team or user, the values cursor_private_worker_pool uses. Null when the API does not report a scope and the list was not filtered by one."},
					"pool_name":                    schema.StringAttribute{Computed: true, Description: "Pool name. Automations select it with private_worker.labels.pool."},
					"repo_owner":                   schema.StringAttribute{Computed: true, Description: "Repository owner of a repo-backed pool. Null for an any-repo pool."},
					"repo_name":                    schema.StringAttribute{Computed: true, Description: "Repository name of a repo-backed pool. Null for an any-repo pool."},
					"repo_url":                     schema.StringAttribute{Computed: true, Description: "Repository URL the pool was registered with. Null when the API returns none."},
					"connected_worker_count":       schema.Int64Attribute{Computed: true, Description: "Workers currently connected to this pool."},
					"in_use_worker_count":          schema.Int64Attribute{Computed: true, Description: "Connected workers that currently have an assigned agent."},
					"worker_ready_timeout_seconds": schema.Int64Attribute{Computed: true, Description: "Seconds an agent waits for a worker in this pool to become ready."},
					"is_stale":                     schema.BoolAttribute{Computed: true, Description: "True when the API reports the pool as stale."},
				}},
			},
		},
	}
}

func (d *privateWorkerPoolsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client, resp.Diagnostics = configureDataClient(req, resp.Diagnostics)
}

func (d *privateWorkerPoolsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg privateWorkerPoolsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePools(d.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	scope := "all"
	if !cfg.Scope.IsNull() && !cfg.Scope.IsUnknown() && cfg.Scope.ValueString() != "" {
		scope = poolListScope(cfg.Scope.ValueString())
	}
	pools, err := client.ListPools(ctx, scope)
	if err != nil {
		resp.Diagnostics.AddError("Failed to list private worker pools", err.Error())
		return
	}
	// scope is optional and not computed, so an omitted value must stay null.
	// The list request still uses all, which returns every pool.
	model := privateWorkerPoolsModel{ID: types.StringValue(scope), Scope: cfg.Scope, Pools: make([]privateWorkerPoolDataModel, 0, len(pools))}
	for _, pool := range pools {
		item := privateWorkerPoolDataModel{
			Scope:                     stringOrNull(poolResourceScope(pool.Scope, scope)),
			PoolName:                  types.StringValue(pool.PoolName),
			RepoOwner:                 types.StringNull(),
			RepoName:                  types.StringNull(),
			RepoURL:                   stringOrNull(pool.RepoURL),
			ConnectedWorkerCount:      types.Int64Value(pool.ConnectedWorkerCount),
			InUseWorkerCount:          types.Int64Value(pool.InUseWorkerCount),
			WorkerReadyTimeoutSeconds: types.Int64Value(pool.WorkerReadyTimeoutSeconds),
			IsStale:                   types.BoolValue(pool.IsStale),
		}
		if pool.RepoOwner != "" {
			item.RepoOwner = types.StringValue(pool.RepoOwner)
			item.RepoName = types.StringValue(pool.RepoName)
		}
		item.ID = types.StringValue(poolTerraformID(privateWorkerPoolModel{Scope: item.Scope, PoolName: item.PoolName, RepoOwner: item.RepoOwner, RepoName: item.RepoName}))
		model.Pools = append(model.Pools, item)
	}
	slices.SortStableFunc(model.Pools, func(a, b privateWorkerPoolDataModel) int {
		return cmp.Compare(a.ID.ValueString(), b.ID.ValueString())
	})
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// poolResourceScope turns the scope the list reports into the value
// cursor_private_worker_pool uses, so a listed id matches the resource id. Which
// spelling the list returns is not documented; both are accepted, and a pool with
// no scope takes it from the list filter when the filter names one.
func poolResourceScope(apiScope, listScope string) string {
	if apiScope == "" {
		apiScope = listScope
	}
	switch apiScope {
	case "team", "team_pool":
		return "team"
	case "user", "personal":
		return "user"
	case "all":
		return ""
	default:
		return apiScope
	}
}

type privateWorkersSummaryDataSource struct{ client *apiClient }

type privateWorkersSummaryModel struct {
	ID                 types.String `tfsdk:"id"`
	TeamTotalConnected types.Int64  `tfsdk:"team_total_connected"`
	TeamInUse          types.Int64  `tfsdk:"team_in_use"`
	UserTotalConnected types.Int64  `tfsdk:"user_total_connected"`
	UserInUse          types.Int64  `tfsdk:"user_in_use"`
}

func NewPrivateWorkersSummaryDataSource() datasource.DataSource {
	return &privateWorkersSummaryDataSource{}
}

func (d *privateWorkersSummaryDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_private_workers_summary"
}

func (d *privateWorkersSummaryDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads connected and in-use private worker counts for the authenticated user and their team, with the provider pool_api_key. The counts change as workers connect and take work, so a value read during plan can differ at apply.",
		Attributes: map[string]schema.Attribute{
			"id":                   schema.StringAttribute{Computed: true, Description: "summary."},
			"team_total_connected": schema.Int64Attribute{Computed: true, Description: "Workers connected to the team's pools. Null when the API returns no team summary."},
			"team_in_use":          schema.Int64Attribute{Computed: true, Description: "Connected team workers that have an assigned agent. Null when the API returns no team summary."},
			"user_total_connected": schema.Int64Attribute{Computed: true, Description: "Workers connected to the user's pools. Null when the API returns no user summary."},
			"user_in_use":          schema.Int64Attribute{Computed: true, Description: "Connected user workers that have an assigned agent. Null when the API returns no user summary."},
		},
	}
}

func (d *privateWorkersSummaryDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client, resp.Diagnostics = configureDataClient(req, resp.Diagnostics)
}

func (d *privateWorkersSummaryDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	client, err := requirePools(d.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	summary, err := client.WorkerSummary(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read private worker summary", err.Error())
		return
	}
	model := privateWorkersSummaryModel{
		ID:                 types.StringValue("summary"),
		TeamTotalConnected: types.Int64Null(),
		TeamInUse:          types.Int64Null(),
		UserTotalConnected: types.Int64Null(),
		UserInUse:          types.Int64Null(),
	}
	if summary.TeamSummary != nil {
		model.TeamTotalConnected = types.Int64Value(summary.TeamSummary.TotalConnected)
		model.TeamInUse = types.Int64Value(summary.TeamSummary.InUse)
	}
	if summary.UserSummary != nil {
		model.UserTotalConnected = types.Int64Value(summary.UserSummary.TotalConnected)
		model.UserInUse = types.Int64Value(summary.UserSummary.InUse)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

func configureDataClient(req datasource.ConfigureRequest, diags diag.Diagnostics) (*apiClient, diag.Diagnostics) {
	if req.ProviderData == nil {
		return nil, diags
	}
	client, ok := req.ProviderData.(*apiClient)
	if !ok {
		diags.AddError("Unexpected provider data type", fmt.Sprintf("Expected *apiClient, got %T", req.ProviderData))
		return nil, diags
	}
	return client, diags
}
