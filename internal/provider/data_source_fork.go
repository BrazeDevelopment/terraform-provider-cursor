package provider

import (
	"context"
	"fmt"
	"strconv"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

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
		UserEmail:     types.StringNull(),
		UserFirstName: types.StringNull(),
		UserLastName:  types.StringNull(),
	}
	if info.UserID != nil {
		model.UserID = types.StringValue(strconv.FormatInt(*info.UserID, 10))
	}
	if info.UserEmail != "" {
		model.UserEmail = types.StringValue(info.UserEmail)
	}
	if info.UserFirstName != "" {
		model.UserFirstName = types.StringValue(info.UserFirstName)
	}
	if info.UserLastName != "" {
		model.UserLastName = types.StringValue(info.UserLastName)
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
		Description: "Reads Admin API team members. role is member or owner. There is no admin value. Use a lifecycle precondition, not a check block, to fail the plan when membership does not match the approved list.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, Description: "team."},
			"members": schema.ListNestedAttribute{
				Computed:    true,
				Description: "Team members, including removed members.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":         schema.StringAttribute{Computed: true},
					"email":      schema.StringAttribute{Computed: true},
					"name":       schema.StringAttribute{Computed: true},
					"role":       schema.StringAttribute{Computed: true, Description: "member or owner."},
					"is_removed": schema.BoolAttribute{Computed: true},
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
		resp.Diagnostics.AddError("Provider not configured", "Team Admin API client is unavailable.")
		return
	}
	members, err := d.client.listTeamMemberRecords(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read team members", err.Error())
		return
	}
	model := teamMembersModel{ID: types.StringValue("team")}
	for _, member := range members {
		model.Members = append(model.Members, teamMemberModel{
			ID:        types.StringValue(member.ID),
			Email:     types.StringValue(member.Email),
			Name:      types.StringValue(member.Name),
			Role:      types.StringValue(member.Role),
			IsRemoved: types.BoolValue(member.IsRemoved),
		})
	}
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
}

func NewEnvironmentDataSource() datasource.DataSource { return &environmentDataSource{} }

func (d *environmentDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment"
}

func (d *environmentDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads one Cloud Agent environment, including environmentJson and versionId. List items omit those fields.",
		Attributes: map[string]schema.Attribute{
			"id":               schema.StringAttribute{Computed: true},
			"environment_id":   schema.StringAttribute{Required: true, Description: "Environment UUID."},
			"name":             schema.StringAttribute{Computed: true},
			"owner":            schema.StringAttribute{Computed: true},
			"environment_json": schema.StringAttribute{Computed: true},
			"version_id":       schema.StringAttribute{Computed: true},
			"repo_file_url":    schema.StringAttribute{Computed: true, Description: "Set when the environment loads its configuration from a file in a repository."},
			"repo_file_path":   schema.StringAttribute{Computed: true},
			"repos": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"url": schema.StringAttribute{Computed: true},
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
	env, err := client.GetEnvironment(ctx, cfg.EnvironmentID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read Cursor environment", err.Error())
		return
	}
	model := environmentDataFromAPI(env)
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

func environmentDataFromAPI(env *publicapi.Environment) environmentDataModel {
	repos := make([]environmentRepoModel, 0, len(env.Repos))
	for _, repo := range env.Repos {
		repos = append(repos, environmentRepoModel{URL: types.StringValue(repo.URL)})
	}
	model := environmentDataModel{
		ID:              types.StringValue(env.ID),
		EnvironmentID:   types.StringValue(env.ID),
		Name:            types.StringNull(),
		Owner:           types.StringValue(env.Owner),
		Repos:           repos,
		EnvironmentJSON: types.StringNull(),
		VersionID:       types.StringNull(),
		RepoFileURL:     types.StringNull(),
		RepoFilePath:    types.StringNull(),
	}
	if env.Name != "" {
		model.Name = types.StringValue(env.Name)
	}
	if env.EnvironmentJSON != "" {
		model.EnvironmentJSON = types.StringValue(env.EnvironmentJSON)
	}
	if env.VersionID != "" {
		model.VersionID = types.StringValue(env.VersionID)
	}
	if env.RepoFile != nil {
		model.RepoFileURL = types.StringValue(env.RepoFile.URL)
		model.RepoFilePath = types.StringValue(env.RepoFile.Path)
	}
	return model
}

type environmentsDataSource struct{ client *apiClient }

type environmentListItemModel struct {
	ID        types.String           `tfsdk:"id"`
	Name      types.String           `tfsdk:"name"`
	Owner     types.String           `tfsdk:"owner"`
	Repos     []environmentRepoModel `tfsdk:"repos"`
	CreatedAt types.String           `tfsdk:"created_at"`
	UpdatedAt types.String           `tfsdk:"updated_at"`
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
		Description: "Lists Cloud Agent environments, following nextCursor until it is absent. A page can be shorter than the limit. An environment updated during the walk can move to the front, so a later page can skip it or repeat another. The data source returns the walk it completed. List items omit environmentJson and versionId.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true},
			"environments": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":         schema.StringAttribute{Computed: true},
					"name":       schema.StringAttribute{Computed: true},
					"owner":      schema.StringAttribute{Computed: true},
					"created_at": schema.StringAttribute{Computed: true},
					"updated_at": schema.StringAttribute{Computed: true},
					"repos": schema.ListNestedAttribute{Computed: true, NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
						"url": schema.StringAttribute{Computed: true},
					}}},
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
	model := environmentsModel{ID: types.StringValue("environments")}
	for _, env := range envs {
		item := environmentListItemModel{
			ID:        types.StringValue(env.ID),
			Name:      types.StringNull(),
			Owner:     types.StringValue(env.Owner),
			CreatedAt: types.StringValue(env.CreatedAt),
			UpdatedAt: types.StringValue(env.UpdatedAt),
		}
		if env.Name != "" {
			item.Name = types.StringValue(env.Name)
		}
		for _, repo := range env.Repos {
			item.Repos = append(item.Repos, environmentRepoModel{URL: types.StringValue(repo.URL)})
		}
		model.Environments = append(model.Environments, item)
	}
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
		Description: "Reads what an agent boots from. type is build or universal_image. universal_image means Cursor's default image and has no status. For a build, status comes from GET /v1/environments/{id}/builds/{buildId} and is IN_PROGRESS, SUCCEEDED, FAILED, CANCELLED, or SKIPPED. A configuration change starts that build asynchronously, so a precondition on status cannot pass in the same apply that changed the environment.",
		Attributes: map[string]schema.Attribute{
			"id":             schema.StringAttribute{Computed: true},
			"environment_id": schema.StringAttribute{Required: true},
			"type":           schema.StringAttribute{Computed: true},
			"build_id":       schema.StringAttribute{Computed: true},
			"status":         schema.StringAttribute{Computed: true},
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
	active, err := client.GetActiveBuild(ctx, cfg.EnvironmentID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read active environment build", err.Error())
		return
	}
	model := environmentActiveBuildModel{
		ID:            types.StringValue(cfg.EnvironmentID.ValueString()),
		EnvironmentID: cfg.EnvironmentID,
		Type:          types.StringValue(active.Type),
		BuildID:       types.StringNull(),
		Status:        types.StringNull(),
	}
	if active.Type == "build" && active.BuildID != "" {
		model.BuildID = types.StringValue(active.BuildID)
		build, err := client.GetBuild(ctx, cfg.EnvironmentID.ValueString(), active.BuildID)
		if err != nil {
			resp.Diagnostics.AddError("Failed to read environment build", err.Error())
			return
		}
		model.Status = types.StringValue(build.Status)
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
			"id": schema.StringAttribute{Computed: true},
			"models": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":           schema.StringAttribute{Computed: true},
					"display_name": schema.StringAttribute{Computed: true},
					"description":  schema.StringAttribute{Computed: true},
					"aliases":      schema.ListAttribute{ElementType: types.StringType, Computed: true},
					"parameters": schema.ListNestedAttribute{Computed: true, NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
						"id":           schema.StringAttribute{Computed: true},
						"display_name": schema.StringAttribute{Computed: true},
						"values": schema.ListNestedAttribute{Computed: true, NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"value":        schema.StringAttribute{Computed: true},
							"display_name": schema.StringAttribute{Computed: true},
						}}},
					}}},
					"variants": schema.ListNestedAttribute{Computed: true, NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
						"display_name": schema.StringAttribute{Computed: true},
						"description":  schema.StringAttribute{Computed: true},
						"is_default":   schema.BoolAttribute{Computed: true},
						"params": schema.ListNestedAttribute{Computed: true, NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"id":    schema.StringAttribute{Computed: true},
							"value": schema.StringAttribute{Computed: true},
						}}},
					}}},
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
	state := modelsModel{ID: types.StringValue("models")}
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
			Description: types.StringNull(),
			Aliases:     aliasList,
		}
		if model.Description != "" {
			item.Description = types.StringValue(model.Description)
		}
		for _, param := range model.Parameters {
			p := modelParameterDataModel{ID: types.StringValue(param.ID), DisplayName: types.StringNull()}
			if param.DisplayName != "" {
				p.DisplayName = types.StringValue(param.DisplayName)
			}
			for _, value := range param.Values {
				v := modelParameterValueModel{Value: types.StringValue(value.Value), DisplayName: types.StringNull()}
				if value.DisplayName != "" {
					v.DisplayName = types.StringValue(value.DisplayName)
				}
				p.Values = append(p.Values, v)
			}
			item.Parameters = append(item.Parameters, p)
		}
		for _, variant := range model.Variants {
			v := modelVariantModel{
				DisplayName: types.StringValue(variant.DisplayName),
				Description: types.StringNull(),
				IsDefault:   types.BoolValue(variant.IsDefault),
			}
			if variant.Description != "" {
				v.Description = types.StringValue(variant.Description)
			}
			for _, param := range variant.Params {
				v.Params = append(v.Params, modelVariantParamModel{ID: types.StringValue(param.ID), Value: types.StringValue(param.Value)})
			}
			item.Variants = append(item.Variants, v)
		}
		state.Models = append(state.Models, item)
	}
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
		Description: "Lists private worker pools. scope is team_pool, personal, or all. The resource scope team maps to team_pool and user maps to personal.",
		Attributes: map[string]schema.Attribute{
			"id":    schema.StringAttribute{Computed: true},
			"scope": schema.StringAttribute{Optional: true, Description: "team_pool, personal, or all. Defaults to all."},
			"pools": schema.ListNestedAttribute{Computed: true, NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
				"id":                           schema.StringAttribute{Computed: true},
				"scope":                        schema.StringAttribute{Computed: true},
				"pool_name":                    schema.StringAttribute{Computed: true},
				"repo_owner":                   schema.StringAttribute{Computed: true},
				"repo_name":                    schema.StringAttribute{Computed: true},
				"repo_url":                     schema.StringAttribute{Computed: true},
				"connected_worker_count":       schema.Int64Attribute{Computed: true},
				"in_use_worker_count":          schema.Int64Attribute{Computed: true},
				"worker_ready_timeout_seconds": schema.Int64Attribute{Computed: true},
				"is_stale":                     schema.BoolAttribute{Computed: true},
			}}},
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
		scope = cfg.Scope.ValueString()
	}
	pools, err := client.ListPools(ctx, scope)
	if err != nil {
		resp.Diagnostics.AddError("Failed to list private worker pools", err.Error())
		return
	}
	model := privateWorkerPoolsModel{ID: types.StringValue(scope), Scope: types.StringValue(scope)}
	for _, pool := range pools {
		item := privateWorkerPoolDataModel{
			ID:                        types.StringValue(poolTerraformID(privateWorkerPoolModel{Scope: types.StringValue(pool.Scope), PoolName: types.StringValue(pool.PoolName), RepoOwner: types.StringValue(pool.RepoOwner), RepoName: types.StringValue(pool.RepoName)})),
			Scope:                     types.StringValue(pool.Scope),
			PoolName:                  types.StringValue(pool.PoolName),
			RepoOwner:                 types.StringNull(),
			RepoName:                  types.StringNull(),
			RepoURL:                   types.StringNull(),
			ConnectedWorkerCount:      types.Int64Value(pool.ConnectedWorkerCount),
			InUseWorkerCount:          types.Int64Value(pool.InUseWorkerCount),
			WorkerReadyTimeoutSeconds: types.Int64Value(pool.WorkerReadyTimeoutSeconds),
			IsStale:                   types.BoolValue(pool.IsStale),
		}
		if pool.RepoOwner != "" {
			item.RepoOwner = types.StringValue(pool.RepoOwner)
			item.RepoName = types.StringValue(pool.RepoName)
		}
		if pool.RepoURL != "" {
			item.RepoURL = types.StringValue(pool.RepoURL)
		}
		model.Pools = append(model.Pools, item)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
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
		Description: "Reads connected and in-use private worker counts for the authenticated user and their team.",
		Attributes: map[string]schema.Attribute{
			"id":                   schema.StringAttribute{Computed: true},
			"team_total_connected": schema.Int64Attribute{Computed: true},
			"team_in_use":          schema.Int64Attribute{Computed: true},
			"user_total_connected": schema.Int64Attribute{Computed: true},
			"user_in_use":          schema.Int64Attribute{Computed: true},
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
