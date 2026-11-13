package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
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

var (
	_ resource.Resource                = (*secretResource)(nil)
	_ resource.ResourceWithImportState = (*secretResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*secretResource)(nil)
)

type secretScope int

const (
	secretScopeEnvironment secretScope = iota
	secretScopeTeam
)

const (
	maxSecretValueBytes = 4096
	secretTypeBuild     = "build_secret"
)

// A 201 from PUT can leave out the version id until the version is readable, so
// the list is read again after each of these waits before create gives up.
var secretVersionPollDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

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
			Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
		},
		"type": schema.StringAttribute{
			Required:    true,
			Description: "runtime_secret or environment_variable. build_secret cannot be set or changed through the API. A version changed to a Build Secret outside Terraform can be destroyed or removed from state, and cannot be updated or imported.",
			Validators:  []validator.String{stringvalidator.OneOf("runtime_secret", "environment_variable")},
		},
		"repos": schema.ListAttribute{
			ElementType: types.StringType,
			Required:    true,
			Description: "Repositories that receive this version, at most 100. An empty list means every repository. A blank entry is rejected so that it cannot turn the list into an empty one. Values are compared as lowercase host/owner/repo, sorted. acme/api, an https URL, and a git@ URL normalize to the same repository.",
			Validators: []validator.List{
				listvalidator.SizeAtMost(100),
				listvalidator.ValueStringsAre(secretRepoValidator{}),
			},
		},
		"value_wo": schema.StringAttribute{
			Required:    true,
			WriteOnly:   true,
			Sensitive:   true,
			Description: "Secret value, 1 to 4,096 bytes. Write-only: it is never stored in state or plan output. Every update sends it, including a change of only type or repos. Requires Terraform or OpenTofu 1.11 or later.",
			Validators:  []validator.String{secretValueValidator{}},
		},
		"value_wo_version": schema.Int64Attribute{
			Required:    true,
			Description: "Change this number to rotate the value when type and repos stay the same. Refresh can see deletion and changes to type or repos, and it cannot see value changes. An imported secret has no number in state, so the first apply after import sends value_wo.",
		},
		"secret_version_id": schema.StringAttribute{
			Computed:    true,
			Description: "Version id sent as id on update and delete. A version keeps this id through changes to its value, type, or repositories.",
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.UseStateForUnknown(),
			},
		},
	}
	desc := "Manages one version of a team Cloud Agents secret. This is the secret production pool workers can receive, and only when Secret sync is on and the worker was started with --sync-dashboard-secrets. Create fails when the name already has a version, because the API would change that version in place; import it instead. Import with <name>/<secret_version_id>, or with <name> when the name has one version. An import id without a version fails when the name has several versions. The first apply after import sends value_wo. Requires Terraform or OpenTofu 1.11 or later."
	if r.scope == secretScopeEnvironment {
		desc = "Manages one version of an environment Cloud Agents secret, identified by secret_version_id. Create fails when the name already has a version in the environment, because the API would change that version in place; import it instead. Import with <environment_id>/<name>/<secret_version_id>, or with <environment_id>/<name> when the name has one version. An import id without a version fails when the name has several versions. The first apply after import sends value_wo. Requires Terraform or OpenTofu 1.11 or later."
		attrs["environment_id"] = schema.StringAttribute{
			Required:    true,
			Description: "Environment that owns the secret. Changing it replaces the resource.",
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.RequiresReplace(),
			},
			Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
		}
	}
	resp.Schema = schema.Schema{Description: desc, Attributes: attrs}
}

// secretValueValidator reports neither the value nor its length.
type secretValueValidator struct{}

func (v secretValueValidator) Description(context.Context) string {
	return fmt.Sprintf("Must be 1 to %d bytes.", maxSecretValueBytes)
}

func (v secretValueValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v secretValueValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if size := len(req.ConfigValue.ValueString()); size == 0 || size > maxSecretValueBytes {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid secret value", fmt.Sprintf("value_wo must be 1 to %d bytes of UTF-8 text.", maxSecretValueBytes))
	}
}

type secretRepoValidator struct{}

func (v secretRepoValidator) Description(context.Context) string {
	return "Must not be blank."
}

func (v secretRepoValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v secretRepoValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if publicapi.CanonicalRepo(req.ConfigValue.ValueString()) == "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid repository", "A repository must not be blank. An empty repos list is the way to give the secret to every repository.")
	}
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
		resp.Diagnostics.AddAttributeError(path.Root("value_wo"), "Missing secret value", "value_wo is required.")
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
	// A PUT without a version id changes the name's existing version in place and
	// never adds a second one, so creating over an existing name would overwrite a
	// secret this resource does not manage.
	existing, err := r.list(ctx, client, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to create Cursor secret", "Listing the existing secrets failed: "+err.Error())
		return
	}
	if ids := secretVersionIDs(existing, plan.Name.ValueString()); len(ids) > 0 {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Cursor secret already exists", fmt.Sprintf("%s already has %d version(s): %s. The API changes an existing version in place and cannot add a second version to a name, so creating this resource would overwrite it. Import the version instead, with the import id %s.", plan.Name.ValueString(), len(ids), strings.Join(ids, ", "), r.importID(plan, ids[0])))
		return
	}
	written, status, err := r.put(ctx, client, plan, "", publicapi.SetSecretRequest{Value: value, Type: plan.Type.ValueString(), Repos: repos})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create Cursor secret", err.Error())
		return
	}
	if status == http.StatusOK {
		resp.Diagnostics.AddWarning("Cursor secret existed before create", fmt.Sprintf("The API reported that it changed an existing version of %s instead of creating one. Either a retried request had already created it, or another writer created it at the same time. This resource now manages that version.", plan.Name.ValueString()))
	}
	versionID, err := r.resolveVersionID(ctx, client, plan, written)
	if err != nil {
		// The secret exists. Recording it without a version id keeps it under
		// management: refresh finds the version by name, and the next apply replaces it.
		resp.Diagnostics.Append(r.setSecret(ctx, &resp.State, secretWritten(plan, ""))...)
		resp.Diagnostics.AddError("Failed to read Cursor secret after creating it", secretErrorSentence(err)+" The secret was created and is recorded in state without a version id; the next apply replaces it.")
		return
	}
	// State keeps the planned values, which Terraform requires after apply. A value
	// the API stored differently shows up as drift on the next refresh.
	resp.Diagnostics.Append(r.setSecret(ctx, &resp.State, secretWritten(plan, versionID))...)
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
	// Only an environment can be gone. A 404 from the team list is a failed read,
	// and treating it as deletion would drop every team secret from state.
	if publicapi.IsNotFound(err) && r.scope == secretScopeEnvironment {
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
	// ImportState leaves type null and every other path writes it, so a null type
	// marks the first read after import. A version id that belongs to another name
	// is a mistyped import id, not a rename.
	if state.Type.IsNull() && version.Name != state.Name.ValueString() {
		resp.Diagnostics.AddError("Secret version does not match the import ID", fmt.Sprintf("Version %s belongs to the secret named %s, not %s. Names match exactly, letter case included.", version.ID, version.Name, state.Name.ValueString()))
		return
	}
	if version.Type == secretTypeBuild {
		if state.Type.IsNull() {
			resp.Diagnostics.AddError("Build Secrets cannot be imported", fmt.Sprintf("Version %s of %s is a Build Secret. The API can delete a Build Secret and cannot set or update one, so this resource cannot manage it.", version.ID, version.Name))
			return
		}
		// Refresh records the type instead of failing, so that destroying the
		// resource or removing it from state still works. ModifyPlan rejects updates.
		resp.Diagnostics.AddWarning("Secret is now a Build Secret", fmt.Sprintf("Version %s of %s was changed to a Build Secret outside Terraform. The API can delete a Build Secret and cannot update one. Terraform can destroy this resource or remove it from state; an update fails until the type is changed back outside Terraform.", version.ID, version.Name))
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
		resp.Diagnostics.AddAttributeError(path.Root("value_wo"), "Missing secret value", "value_wo is required on every update, including a change of only type or repos.")
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
	written, _, err := r.put(ctx, client, plan, versionID, publicapi.SetSecretRequest{Value: value, Type: plan.Type.ValueString(), Repos: repos})
	if publicapi.IsNotFound(err) {
		resp.Diagnostics.AddError("Cursor secret no longer exists", secretErrorSentence(err)+" The secret version or its environment was deleted outside Terraform. Run terraform apply again to create it.")
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to update Cursor secret", err.Error())
		return
	}
	// The planned id is the prior one, and a version keeps its id through every change.
	if versionID == "" {
		versionID, err = r.resolveVersionID(ctx, client, plan, written)
		if err != nil {
			resp.Diagnostics.AddError("Failed to read Cursor secret after updating it", err.Error())
			return
		}
	}
	resp.Diagnostics.Append(r.setSecret(ctx, &resp.State, secretWritten(plan, versionID))...)
}

// ModifyPlan rejects an in-place update of a version that refresh found to be a
// Build Secret. Destroy plans pass, and so do replacements, which delete the Build
// Secret and create the configured version.
func (r *secretResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return
	}
	var kind types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("type"), &kind)...)
	if resp.Diagnostics.HasError() || kind.ValueString() != secretTypeBuild {
		return
	}
	replacing := []string{"name"}
	if r.scope == secretScopeEnvironment {
		replacing = append(replacing, "environment_id")
	}
	for _, attribute := range replacing {
		var prior, planned types.String
		resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root(attribute), &prior)...)
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root(attribute), &planned)...)
		if resp.Diagnostics.HasError() || !planned.Equal(prior) {
			return
		}
	}
	resp.Diagnostics.AddAttributeError(path.Root("type"), "Build Secrets cannot be updated", "This secret version was changed to a Build Secret outside Terraform. The API can delete a Build Secret and cannot update one. Change the type back outside Terraform, remove the resource from state, or destroy it; applying again after a destroy creates the configured version.")
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

func (r *secretResource) importID(model secretModel, versionID string) string {
	if r.scope == secretScopeTeam {
		return model.Name.ValueString() + "/" + versionID
	}
	return model.EnvironmentID.ValueString() + "/" + model.Name.ValueString() + "/" + versionID
}

// resolveVersionID returns the id of the version a PUT wrote. When the response
// left it out, the version is looked up by name, waiting for it to become readable.
func (r *secretResource) resolveVersionID(ctx context.Context, client *publicapi.Client, model secretModel, written *publicapi.SecretVersion) (string, error) {
	if written != nil && written.ID != "" {
		return written.ID, nil
	}
	for attempt := 0; ; attempt++ {
		versions, err := r.list(ctx, client, model)
		if err != nil {
			return "", err
		}
		found, ambiguous := findSecretVersion(versions, model.Name.ValueString(), "")
		if ambiguous != nil {
			return "", ambiguous
		}
		if found != nil && found.ID != "" {
			return found.ID, nil
		}
		if attempt >= len(secretVersionPollDelays) {
			return "", fmt.Errorf("the API did not return a secret version id, and the list does not contain a version named %s yet", model.Name.ValueString())
		}
		if err := sleepContext(ctx, secretVersionPollDelays[attempt]); err != nil {
			return "", fmt.Errorf("waiting for the secret version to become readable: %w", err)
		}
	}
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

func secretVersionIDs(versions []publicapi.SecretVersion, name string) []string {
	var ids []string
	for _, version := range versions {
		if version.Name == name {
			ids = append(ids, version.ID)
		}
	}
	return ids
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

// secretState is the state after a refresh: the API's view, with the configured
// spelling of repos kept while it names the same repositories.
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

// secretWritten is the state after a successful PUT: the planned values and the
// version id. An empty id is recorded as null.
func secretWritten(plan secretModel, versionID string) secretModel {
	state := plan
	state.ValueWO = types.StringNull()
	state.ID = types.StringNull()
	if versionID != "" {
		state.ID = types.StringValue(versionID)
	}
	state.SecretVersionID = state.ID
	return state
}

// secretErrorSentence returns the error text ending in a period, so more text can follow it.
func secretErrorSentence(err error) string {
	return strings.TrimRight(err.Error(), ". ") + "."
}

func secretValue(cfg secretModel) (string, bool) {
	if cfg.ValueWO.IsNull() || cfg.ValueWO.IsUnknown() || cfg.ValueWO.ValueString() == "" {
		return "", false
	}
	return cfg.ValueWO.ValueString(), true
}

// secretRepos returns the repositories to send. An empty result gives the secret
// to every repository, so only an explicitly empty list may produce one.
func secretRepos(ctx context.Context, list types.List) ([]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if list.IsNull() || list.IsUnknown() {
		diags.AddAttributeError(path.Root("repos"), "Missing repositories", "repos must be a known list. Use an empty list to give the secret to every repository.")
		return nil, diags
	}
	var repos []string
	diags.Append(list.ElementsAs(ctx, &repos, false)...)
	if diags.HasError() {
		return nil, diags
	}
	for i, repo := range repos {
		if publicapi.CanonicalRepo(repo) == "" {
			diags.AddAttributeError(path.Root("repos").AtListIndex(i), "Invalid repository", "A repository must not be blank. An empty repos list is the way to give the secret to every repository.")
		}
	}
	return publicapi.CanonicalRepos(repos), diags
}

func parseSecretImport(scope secretScope, id string) (envID, name, versionID string, err error) {
	format := "<name> or <name>/<secret_version_id>"
	if scope == secretScopeEnvironment {
		format = "<environment_id>/<name> or <environment_id>/<name>/<secret_version_id>"
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", "", "", fmt.Errorf("import id is empty; expected %s", format)
	}
	parts := strings.Split(id, "/")
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return "", "", "", fmt.Errorf("expected %s, got %q, which has an empty segment", format, id)
		}
	}
	if scope == secretScopeEnvironment {
		if len(parts) < 2 {
			return "", "", "", fmt.Errorf("expected %s, got %q", format, id)
		}
		envID, parts = parts[0], parts[1:]
	}
	if last := len(parts) - 1; last >= 1 && strings.HasPrefix(parts[last], "secv_") {
		versionID, parts = parts[last], parts[:last]
	}
	return envID, strings.Join(parts, "/"), versionID, nil
}
