package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type secretScope int

const (
	secretScopeEnvironment secretScope = iota
	secretScopeTeam
)

type secretResource struct {
	client *apiClient
	scope  secretScope
}

type secretModel struct {
	ID              types.String `tfsdk:"id"`
	EnvironmentID   types.String `tfsdk:"environment_id"`
	Name            types.String `tfsdk:"name"`
	Type            types.String `tfsdk:"type"`
	Repos           types.List   `tfsdk:"repos"`
	ValueWO         types.String `tfsdk:"value_wo"`
	ValueWOVersion  types.Int64  `tfsdk:"value_wo_version"`
	SecretVersionID types.String `tfsdk:"secret_version_id"`
}

// teamSecretModel matches cursor_team_secret, which has no environment_id.
type teamSecretModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	Type            types.String `tfsdk:"type"`
	Repos           types.List   `tfsdk:"repos"`
	ValueWO         types.String `tfsdk:"value_wo"`
	ValueWOVersion  types.Int64  `tfsdk:"value_wo_version"`
	SecretVersionID types.String `tfsdk:"secret_version_id"`
}

type attrGetter interface {
	Get(context.Context, any) diag.Diagnostics
}

func NewEnvironmentSecretResource() resource.Resource {
	return &secretResource{scope: secretScopeEnvironment}
}

func NewTeamSecretResource() resource.Resource {
	return &secretResource{scope: secretScopeTeam}
}

func (r *secretResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	if r.scope == secretScopeTeam {
		resp.TypeName = req.ProviderTypeName + "_team_secret"
		return
	}
	resp.TypeName = req.ProviderTypeName + "_environment_secret"
}

func (r *secretResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:    true,
			Description: "Secret version id.",
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.UseStateForUnknown(),
			},
		},
		"name": schema.StringAttribute{
			Required:    true,
			Description: "Secret name. Changing it replaces the resource. Names that differ only by case conflict.",
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.RequiresReplace(),
			},
		},
		"type": schema.StringAttribute{
			Required:    true,
			Description: "runtime_secret or environment_variable. build_secret cannot be set or changed through the API.",
			Validators:  []validator.String{stringvalidator.OneOf("runtime_secret", "environment_variable")},
		},
		"repos": schema.ListAttribute{
			ElementType: types.StringType,
			Required:    true,
			Description: "Repositories that receive this version. An empty list means every repository. Values are compared as lowercase host/owner/repo, sorted. acme/api, an https URL, and a git@ URL normalize to the same repository.",
		},
		"value_wo": schema.StringAttribute{
			Required:    true,
			WriteOnly:   true,
			Sensitive:   true,
			Description: "Secret value. Write-only: it is never stored in state or plan output. Every update sends it, including a change of only type or repos. Requires Terraform or OpenTofu 1.11 or later.",
		},
		"value_wo_version": schema.Int64Attribute{
			Required:    true,
			Description: "Change this number to rotate the value when type and repos stay the same. Refresh can see deletion and changes to type or repos, and it cannot see value changes.",
		},
		"secret_version_id": schema.StringAttribute{
			Computed:    true,
			Description: "Version id sent as id on update and delete. A version keeps this id through changes to its value, type, or repositories.",
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.UseStateForUnknown(),
			},
		},
	}
	desc := "Manages one version of a team Cloud Agents secret. This is the secret production pool workers can receive, and only when Secret sync is on and the worker was started with --sync-dashboard-secrets. Import with <name>/<secret_version_id>. An import id without a version fails when the name has several versions."
	if r.scope == secretScopeEnvironment {
		desc = "Manages one version of an environment Cloud Agents secret, identified by secret_version_id. Import with <environment_id>/<name>/<secret_version_id>. An import id without a version fails when the name has several versions. Requires Terraform or OpenTofu 1.11 or later."
		attrs["environment_id"] = schema.StringAttribute{
			Required:    true,
			Description: "Environment that owns the secret. Changing it replaces the resource.",
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.RequiresReplace(),
			},
		}
	}
	resp.Schema = schema.Schema{Description: desc, Attributes: attrs}
}

func (r *secretResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *secretResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	plan, diags := r.getSecret(ctx, req.Plan)
	resp.Diagnostics.Append(diags...)
	cfg, diags := r.getSecret(ctx, req.Config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	value, ok := secretValue(cfg)
	if !ok {
		resp.Diagnostics.AddError("Missing secret value", "value_wo is required.")
		return
	}
	repos, diags := secretRepos(ctx, plan.Repos)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePublic(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	written, status, err := r.put(ctx, client, plan, "", publicapi.SetSecretRequest{Value: value, Type: plan.Type.ValueString(), Repos: repos})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create Cursor secret", err.Error())
		return
	}
	version, err := r.resolveVersion(ctx, client, plan, written, status)
	if err != nil {
		resp.Diagnostics.AddError("Failed to create Cursor secret", err.Error())
		return
	}
	resp.Diagnostics.Append(r.setSecret(ctx, &resp.State, secretState(plan, version))...)
}

func (r *secretResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	state, diags := r.getSecret(ctx, req.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePublic(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	versions, err := r.list(ctx, client, state)
	if publicapi.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read Cursor secret", err.Error())
		return
	}
	version, ambiguous := findSecretVersion(versions, state.Name.ValueString(), state.SecretVersionID.ValueString())
	if ambiguous != nil {
		resp.Diagnostics.AddError("Secret name is ambiguous", ambiguous.Error())
		return
	}
	if version == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	if version.Type == "build_secret" {
		resp.Diagnostics.AddError("Build Secrets cannot be updated", "The API can delete a Build Secret and cannot update one. Remove this resource from Terraform or delete the Build Secret out of band.")
		return
	}
	resp.Diagnostics.Append(r.setSecret(ctx, &resp.State, secretState(state, version))...)
}

func (r *secretResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	plan, diags := r.getSecret(ctx, req.Plan)
	resp.Diagnostics.Append(diags...)
	prior, diags := r.getSecret(ctx, req.State)
	resp.Diagnostics.Append(diags...)
	cfg, diags := r.getSecret(ctx, req.Config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	value, ok := secretValue(cfg)
	if !ok {
		resp.Diagnostics.AddError("Missing secret value", "value_wo is required on every update, including a change of only type or repos.")
		return
	}
	repos, diags := secretRepos(ctx, plan.Repos)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePublic(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	versionID := prior.SecretVersionID.ValueString()
	written, status, err := r.put(ctx, client, plan, versionID, publicapi.SetSecretRequest{Value: value, Type: plan.Type.ValueString(), Repos: repos})
	if err != nil {
		resp.Diagnostics.AddError("Failed to update Cursor secret", err.Error())
		return
	}
	if written.ID == "" {
		written.ID = versionID
	}
	version, err := r.resolveVersion(ctx, client, plan, written, status)
	if err != nil {
		resp.Diagnostics.AddError("Failed to update Cursor secret", err.Error())
		return
	}
	resp.Diagnostics.Append(r.setSecret(ctx, &resp.State, secretState(plan, version))...)
}

func (r *secretResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	state, diags := r.getSecret(ctx, req.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	client, err := requirePublic(r.client)
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	err = r.delete(ctx, client, state)
	if err != nil && !publicapi.IsNotFound(err) {
		resp.Diagnostics.AddError("Failed to delete Cursor secret", err.Error())
	}
}

func (r *secretResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	envID, name, versionID, err := parseSecretImport(r.scope, req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	if r.scope == secretScopeEnvironment {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("environment_id"), envID)...)
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	if versionID != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("secret_version_id"), versionID)...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), versionID)...)
	}
}

func (r *secretResource) put(ctx context.Context, client *publicapi.Client, model secretModel, versionID string, body publicapi.SetSecretRequest) (*publicapi.SecretVersion, int, error) {
	if r.scope == secretScopeTeam {
		return client.PutTeamSecret(ctx, model.Name.ValueString(), versionID, body)
	}
	return client.PutEnvironmentSecret(ctx, model.EnvironmentID.ValueString(), model.Name.ValueString(), versionID, body)
}

func (r *secretResource) list(ctx context.Context, client *publicapi.Client, model secretModel) ([]publicapi.SecretVersion, error) {
	if r.scope == secretScopeTeam {
		return client.ListTeamSecrets(ctx)
	}
	return client.ListEnvironmentSecrets(ctx, model.EnvironmentID.ValueString())
}

func (r *secretResource) delete(ctx context.Context, client *publicapi.Client, model secretModel) error {
	if r.scope == secretScopeTeam {
		return client.DeleteTeamSecret(ctx, model.Name.ValueString(), model.SecretVersionID.ValueString())
	}
	return client.DeleteEnvironmentSecret(ctx, model.EnvironmentID.ValueString(), model.Name.ValueString(), model.SecretVersionID.ValueString())
}

func (r *secretResource) resolveVersion(ctx context.Context, client *publicapi.Client, model secretModel, written *publicapi.SecretVersion, status int) (*publicapi.SecretVersion, error) {
	if written != nil && written.ID != "" && written.Type != "" {
		if written.Repos == nil {
			written.Repos = []string{}
		}
		return written, nil
	}
	versions, err := r.list(ctx, client, model)
	if err != nil {
		return nil, err
	}
	wantID := ""
	if written != nil {
		wantID = written.ID
	}
	found, ambiguous := findSecretVersion(versions, model.Name.ValueString(), wantID)
	if ambiguous != nil {
		return nil, ambiguous
	}
	if found == nil {
		if status == 201 {
			return nil, fmt.Errorf("the API created the secret but did not return a version id, and the list does not contain a single matching version yet")
		}
		return nil, fmt.Errorf("the API did not return a secret version id")
	}
	return found, nil
}

func findSecretVersion(versions []publicapi.SecretVersion, name, id string) (*publicapi.SecretVersion, error) {
	if id != "" {
		for i := range versions {
			if versions[i].ID == id {
				return &versions[i], nil
			}
		}
		return nil, nil
	}
	var matches []publicapi.SecretVersion
	for _, version := range versions {
		if version.Name == name {
			matches = append(matches, version)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		return nil, &publicapi.Error{Status: 409, Code: "secret_name_ambiguous", Message: fmt.Sprintf("%s has %d versions. Pass the id of one as the import version.", name, len(matches)), Versions: versionSummaries(matches)}
	}
}

func versionSummaries(versions []publicapi.SecretVersion) []publicapi.SecretVersionSummary {
	out := make([]publicapi.SecretVersionSummary, 0, len(versions))
	for _, version := range versions {
		out = append(out, publicapi.SecretVersionSummary{ID: version.ID, Repos: version.Repos})
	}
	return out
}

func (r *secretResource) getSecret(ctx context.Context, src attrGetter) (secretModel, diag.Diagnostics) {
	if r.scope == secretScopeTeam {
		var team teamSecretModel
		diags := src.Get(ctx, &team)
		return secretModel{
			ID:              team.ID,
			Name:            team.Name,
			Type:            team.Type,
			Repos:           team.Repos,
			ValueWO:         team.ValueWO,
			ValueWOVersion:  team.ValueWOVersion,
			SecretVersionID: team.SecretVersionID,
		}, diags
	}
	var model secretModel
	diags := src.Get(ctx, &model)
	return model, diags
}

func (r *secretResource) setSecret(ctx context.Context, state *tfsdk.State, model secretModel) diag.Diagnostics {
	model.ValueWO = types.StringNull()
	if r.scope == secretScopeTeam {
		return state.Set(ctx, &teamSecretModel{
			ID:              model.ID,
			Name:            model.Name,
			Type:            model.Type,
			Repos:           model.Repos,
			ValueWO:         model.ValueWO,
			ValueWOVersion:  model.ValueWOVersion,
			SecretVersionID: model.SecretVersionID,
		})
	}
	return state.Set(ctx, &model)
}

func secretState(prior secretModel, version *publicapi.SecretVersion) secretModel {
	state := prior
	state.ValueWO = types.StringNull()
	state.ID = types.StringValue(version.ID)
	state.SecretVersionID = types.StringValue(version.ID)
	state.Name = types.StringValue(version.Name)
	state.Type = types.StringValue(version.Type)
	state.Repos = keepEquivalentRepos(context.Background(), prior.Repos, version.Repos)
	return state
}

func secretValue(cfg secretModel) (string, bool) {
	if cfg.ValueWO.IsNull() || cfg.ValueWO.IsUnknown() || cfg.ValueWO.ValueString() == "" {
		return "", false
	}
	return cfg.ValueWO.ValueString(), true
}

func secretRepos(ctx context.Context, list types.List) ([]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if list.IsNull() || list.IsUnknown() {
		return []string{}, diags
	}
	var repos []string
	diags.Append(list.ElementsAs(ctx, &repos, false)...)
	return publicapi.CanonicalRepos(repos), diags
}

func parseSecretImport(scope secretScope, id string) (envID, name, versionID string, err error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", fmt.Errorf("import id is empty")
	}
	parts := strings.Split(id, "/")
	if scope == secretScopeTeam {
		if len(parts) >= 2 && strings.HasPrefix(parts[len(parts)-1], "secv_") {
			return "", strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1], nil
		}
		return "", id, "", nil
	}
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("expected <environment_id>/<name> or <environment_id>/<name>/<secret_version_id>, got %q", id)
	}
	envID = parts[0]
	if strings.HasPrefix(parts[len(parts)-1], "secv_") && len(parts) >= 3 {
		return envID, strings.Join(parts[1:len(parts)-1], "/"), parts[len(parts)-1], nil
	}
	return envID, strings.Join(parts[1:], "/"), "", nil
}
