package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	modelAccessStateCustom       = "custom"
	modelAccessStateUnrestricted = "unrestricted"
	modelAccessStateLegacy       = "legacy"
)

// Provider and model routes answer 409 in these states, and the configuration has no custom policy to manage.
func modelAccessPolicyOff(state string) bool {
	return state == modelAccessStateUnrestricted || state == modelAccessStateLegacy
}

var (
	modelAccessProviderIDPattern = regexp.MustCompile(`^[^/\s]+$`)
	modelAccessModelIDPattern    = regexp.MustCompile(`^\S+$`)
)

type modelAccessConfigurationResource struct{ client *apiClient }

type modelAccessConfigurationModel struct {
	ID                 types.String `tfsdk:"id"`
	TeamID             types.Int64  `tfsdk:"team_id"`
	State              types.String `tfsdk:"state"`
	NewProviderDefault types.String `tfsdk:"new_provider_default"`
	NewModelDefault    types.String `tfsdk:"new_model_default"`
	DeletionProtection types.Bool   `tfsdk:"deletion_protection"`
}

func NewModelAccessConfigurationResource() resource.Resource {
	return &modelAccessConfigurationResource{}
}

func (r *modelAccessConfigurationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_model_access_configuration"
}

func (r *modelAccessConfigurationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	choice := []validator.String{stringvalidator.OneOf("enabled", "disabled")}
	resp.Schema = schema.Schema{
		Description: "Manages the team's model-access policy. The policy is a singleton: declare this resource once per team, because two declarations overwrite each other. The first apply turns the policy on and seeds the catalog. When a custom policy already exists, create adopts it and sets the two defaults. Provider and model resources return 409 until this resource has been applied. Destroying it sends state unrestricted, which clears the custom policy together with every provider and model setting. deletion_protection defaults to true. When the policy is switched back to unrestricted or legacy outside Terraform, refresh drops the resource from state and the next plan creates it again. Import with the team id or the literal team; import fails when the team has no custom policy. Organization Groups can still allow a model the team policy disables; this API is the team baseline. These ids are Admin catalog ids, a different space from automation model slugs.",
		Attributes: map[string]schema.Attribute{
			"id":                   schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "Team id as a string."},
			"team_id":              schema.Int64Attribute{Computed: true, PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}, Description: "Team id implied by the API key."},
			"state":                schema.StringAttribute{Computed: true, Description: "custom while this resource exists. unrestricted and legacy mean there is no custom policy."},
			"new_provider_default": schema.StringAttribute{Required: true, Description: "enabled or disabled. Default for newly seen providers.", Validators: choice},
			"new_model_default":    schema.StringAttribute{Required: true, Description: "enabled or disabled. Default for newly seen models.", Validators: choice},
			"deletion_protection": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "When true, Terraform will not clear the custom policy. Null is treated as protected.",
			},
		},
	}
}

func (r *modelAccessConfigurationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client, resp.Diagnostics = configureClient(req, resp.Diagnostics)
}

func (r *modelAccessConfigurationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan modelAccessConfigurationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	next, err := r.write(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to update model access configuration", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, next)...)
}

func (r *modelAccessConfigurationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state modelAccessConfigurationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.getModelAccessConfiguration(ctx)
	if err != nil {
		if isAdminNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read model access configuration", err.Error())
		return
	}
	if modelAccessPolicyOff(out.State) {
		resp.Diagnostics.AddWarning("Model access policy is not custom", fmt.Sprintf("The team's model access state is %q, so there is no custom policy. The resource was removed from state and the next apply creates the policy again.", out.State))
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, configurationModel(state, out))...)
}

func (r *modelAccessConfigurationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state modelAccessConfigurationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A deletion_protection flip has nothing to send; a PUT here would rewrite the shared team policy for no reason.
	if plan.NewProviderDefault.Equal(state.NewProviderDefault) && plan.NewModelDefault.Equal(state.NewModelDefault) && !state.ID.IsNull() && !state.State.IsNull() {
		plan.ID, plan.TeamID, plan.State = state.ID, state.TeamID, state.State
		plan.DeletionProtection = boolOrDefault(plan.DeletionProtection, true)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	next, err := r.write(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to update model access configuration", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, next)...)
}

func (r *modelAccessConfigurationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state modelAccessConfigurationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if deletionProtectionEnabled(state.DeletionProtection) {
		resp.Diagnostics.AddError("Model access configuration is protected from deletion", "deletion_protection is enabled; set deletion_protection = false and apply before clearing the custom policy.")
		return
	}
	if _, err := r.client.putModelAccessConfiguration(ctx, map[string]string{"state": modelAccessStateUnrestricted}); err != nil && !isAdminNotFound(err) {
		resp.Diagnostics.AddError("Failed to clear model access configuration", err.Error())
	}
}

func (r *modelAccessConfigurationResource) write(ctx context.Context, plan modelAccessConfigurationModel) (*modelAccessConfigurationModel, error) {
	out, err := r.client.putModelAccessConfiguration(ctx, map[string]string{
		"newProviderDefault": plan.NewProviderDefault.ValueString(),
		"newModelDefault":    plan.NewModelDefault.ValueString(),
	})
	if err != nil {
		return nil, err
	}
	if modelAccessPolicyOff(out.State) {
		return nil, fmt.Errorf("the API still reports state %q after the write, so the custom policy is not on", out.State)
	}
	for name, pair := range map[string][2]string{
		"new_provider_default": {plan.NewProviderDefault.ValueString(), stringPtrValue(out.NewProviderDefault).ValueString()},
		"new_model_default":    {plan.NewModelDefault.ValueString(), stringPtrValue(out.NewModelDefault).ValueString()},
	} {
		if pair[1] != "" && pair[1] != pair[0] {
			return nil, fmt.Errorf("the API reports %s = %q after writing %q", name, pair[1], pair[0])
		}
	}
	next := configurationModel(plan, out)
	// The response may omit the defaults; state for a required attribute has to equal the plan.
	next.NewProviderDefault = plan.NewProviderDefault
	next.NewModelDefault = plan.NewModelDefault
	return next, nil
}

func configurationModel(prior modelAccessConfigurationModel, out *modelAccessConfiguration) *modelAccessConfigurationModel {
	prior.TeamID = types.Int64Value(out.TeamID)
	prior.ID = types.StringValue(strconv.FormatInt(out.TeamID, 10))
	prior.State = types.StringValue(out.State)
	prior.NewProviderDefault = stringPtrValue(out.NewProviderDefault)
	prior.NewModelDefault = stringPtrValue(out.NewModelDefault)
	prior.DeletionProtection = boolOrDefault(prior.DeletionProtection, true)
	return &prior
}

// modelAccessConfigurationImportID checks that there is a custom policy to import and returns the team id.
// A numeric import id has to name the key's team; any other id selects the singleton.
func modelAccessConfigurationImportID(ctx context.Context, client *apiClient, id string) (string, error) {
	id = strings.TrimSpace(id)
	if client == nil {
		return id, nil
	}
	out, err := client.getModelAccessConfiguration(ctx)
	if err != nil {
		return "", err
	}
	if out.State != modelAccessStateCustom {
		return "", fmt.Errorf("the team's model access state is %q, so there is no custom policy to import; declare cursor_model_access_configuration and apply to create it", out.State)
	}
	teamID := strconv.FormatInt(out.TeamID, 10)
	if _, err := strconv.ParseInt(id, 10, 64); err == nil && id != teamID {
		return "", fmt.Errorf("import id %q does not match team %s, the team of the configured team_api_key", id, teamID)
	}
	return teamID, nil
}

// modelAccessAlreadyReset reports whether a failed destroy write can be treated as done: the entry
// is gone, or the write conflicted because the custom policy is confirmed off and no setting is
// left to disable. Any other conflict stays an error so an entry is never dropped while enabled.
func modelAccessAlreadyReset(ctx context.Context, client *apiClient, err error) bool {
	if isAdminNotFound(err) {
		return true
	}
	if !isModelAccessConflict(err) {
		return false
	}
	out, cfgErr := client.getModelAccessConfiguration(ctx)
	return cfgErr == nil && modelAccessPolicyOff(out.State)
}

// modelAccessEntryGone reports whether a provider or model read failed because the entry no longer
// exists: a 404, or a 409 while the configuration confirms the custom policy is off.
func modelAccessEntryGone(ctx context.Context, client *apiClient, err error, diags *diag.Diagnostics) bool {
	if isAdminNotFound(err) {
		return true
	}
	if !isModelAccessConflict(err) {
		return false
	}
	out, cfgErr := client.getModelAccessConfiguration(ctx)
	if cfgErr != nil || !modelAccessPolicyOff(out.State) {
		return false
	}
	diags.AddWarning("Model access policy is not custom", fmt.Sprintf("The team's model access state is %q, so provider and model settings no longer exist. The resource was removed from state; apply cursor_model_access_configuration before it is created again.", out.State))
	return true
}

func isModelAccessConflict(err error) bool {
	var apiErr *adminStatusError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict
}

func stringPtrValue(value *string) types.String {
	if value == nil || *value == "" {
		return types.StringNull()
	}
	return types.StringValue(*value)
}

type modelAccessProviderResource struct{ client *apiClient }

type modelAccessProviderModel struct {
	ID          types.String `tfsdk:"id"`
	ProviderID  types.String `tfsdk:"provider_id"`
	DisplayName types.String `tfsdk:"display_name"`
	Enabled     types.Bool   `tfsdk:"enabled"`
}

func NewModelAccessProviderResource() resource.Resource { return &modelAccessProviderResource{} }

func (r *modelAccessProviderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_model_access_provider"
}

func (r *modelAccessProviderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Enables or disables one model-access provider. Catalog ids look like anthropic. The catalog entry always exists once the policy is on, so create adopts it and sets enabled. Declare each provider once; two declarations overwrite each other. Writes return 409 while configuration state is unrestricted or legacy; apply cursor_model_access_configuration first. Refresh drops the resource from state when the provider leaves the catalog or the custom policy is cleared. Destroy disables the provider; to stop managing it without changing it, remove it from state instead. Import with the provider id.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "Provider catalog id."},
			"provider_id": schema.StringAttribute{
				Required:      true,
				Description:   "Catalog provider id, such as anthropic.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(modelAccessProviderIDPattern, "must not be empty or contain / or whitespace")},
			},
			"display_name": schema.StringAttribute{Computed: true, Description: "Display name returned by the API."},
			"enabled":      schema.BoolAttribute{Required: true, Description: "Whether the provider is enabled."},
		},
	}
}

func (r *modelAccessProviderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client, resp.Diagnostics = configureClient(req, resp.Diagnostics)
}

func (r *modelAccessProviderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan modelAccessProviderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state, err := r.write(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to update model access provider", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *modelAccessProviderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state modelAccessProviderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	next, err := r.read(ctx, state)
	if err != nil {
		if modelAccessEntryGone(ctx, r.client, err, &resp.Diagnostics) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read model access provider", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

func (r *modelAccessProviderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan modelAccessProviderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state, err := r.write(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to update model access provider", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *modelAccessProviderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state modelAccessProviderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.putModelAccessProvider(ctx, state.ProviderID.ValueString(), false); err != nil && !modelAccessAlreadyReset(ctx, r.client, err) {
		resp.Diagnostics.AddError("Failed to disable model access provider", err.Error())
	}
}

func (r *modelAccessProviderResource) write(ctx context.Context, plan modelAccessProviderModel) (modelAccessProviderModel, error) {
	if err := r.client.putModelAccessProvider(ctx, plan.ProviderID.ValueString(), plan.Enabled.ValueBool()); err != nil {
		return plan, err
	}
	next, err := r.read(ctx, plan)
	if err != nil {
		return plan, fmt.Errorf("reading the provider after the write: %w", err)
	}
	if next.Enabled.ValueBool() != plan.Enabled.ValueBool() {
		return plan, fmt.Errorf("the API reports enabled = %t for provider %q after writing %t", next.Enabled.ValueBool(), plan.ProviderID.ValueString(), plan.Enabled.ValueBool())
	}
	return next, nil
}

func (r *modelAccessProviderResource) read(ctx context.Context, model modelAccessProviderModel) (modelAccessProviderModel, error) {
	providers, err := r.client.listModelAccessProviders(ctx)
	if err != nil {
		return model, err
	}
	for _, provider := range providers {
		if provider.ID == model.ProviderID.ValueString() {
			model.ID = types.StringValue(provider.ID)
			model.DisplayName = types.StringValue(provider.DisplayName)
			model.Enabled = types.BoolValue(provider.Enabled)
			return model, nil
		}
	}
	return model, &adminStatusError{API: "Team Admin API", Status: http.StatusNotFound, Message: "provider was not found"}
}

type modelParameterModel struct {
	ID            types.String `tfsdk:"id"`
	AllowedValues types.List   `tfsdk:"allowed_values"`
	DefaultValue  types.String `tfsdk:"default_value"`
}

type modelAccessModelResource struct{ client *apiClient }

type modelAccessModelModel struct {
	ID          types.String          `tfsdk:"id"`
	ProviderID  types.String          `tfsdk:"provider_id"`
	ModelID     types.String          `tfsdk:"model_id"`
	DisplayName types.String          `tfsdk:"display_name"`
	Enabled     types.Bool            `tfsdk:"enabled"`
	Parameters  []modelParameterModel `tfsdk:"parameters"`
}

func NewModelAccessModelResource() resource.Resource { return &modelAccessModelResource{} }

func (r *modelAccessModelResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_model_access_model"
}

func (r *modelAccessModelResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Enables or disables one model and, optionally, its parameter restrictions. Catalog ids look like claude-opus-4-6. The catalog entry always exists once the policy is on, so create adopts it and sets enabled and the listed parameters. Declare each model once; two declarations overwrite each other. Writes return 409 until cursor_model_access_configuration has been applied. Refresh drops the resource from state when the model leaves the catalog or the custom policy is cleared. When parameters is omitted, refresh does not record the catalog parameters. Destroy disables the model and clears the restrictions on the parameters this resource listed; to stop managing it without changing it, remove it from state instead. Import with provider_id/model_id; import records the parameters that carry a team restriction or default.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "provider/model."},
			"provider_id": schema.StringAttribute{
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Description:   "Catalog provider id.",
				Validators:    []validator.String{stringvalidator.RegexMatches(modelAccessProviderIDPattern, "must not be empty or contain / or whitespace")},
			},
			"model_id": schema.StringAttribute{
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Description:   "Catalog model id.",
				Validators:    []validator.String{stringvalidator.RegexMatches(modelAccessModelIDPattern, "must not be empty or contain whitespace")},
			},
			"display_name": schema.StringAttribute{Computed: true, Description: "Display name returned by the API."},
			"enabled":      schema.BoolAttribute{Required: true, Description: "Whether the model is enabled."},
			"parameters": schema.ListNestedAttribute{
				Optional:    true,
				Description: "Per-parameter allowed values and default. Parameters that were never listed are left unchanged. Removing a parameter from the list clears its restriction and default. Null allowed_values clears a restriction. Parameter ids must be unique.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id": schema.StringAttribute{Required: true, Description: "Parameter id, such as fast or reasoning.", Validators: []validator.String{stringvalidator.LengthAtLeast(1)}},
					"allowed_values": schema.ListAttribute{
						ElementType: types.StringType,
						Optional:    true,
						Description: "Values members may pick. Null clears the restriction. Order is not significant.",
						Validators:  []validator.List{listvalidator.UniqueValues(), listvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1))},
					},
					"default_value": schema.StringAttribute{Optional: true, Description: "Team default. Null restores the catalog default."},
				}},
			},
		},
	}
}

func (r *modelAccessModelResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client, resp.Diagnostics = configureClient(req, resp.Diagnostics)
}

var _ resource.ResourceWithValidateConfig = (*modelAccessModelResource)(nil)

func (r *modelAccessModelResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var params types.List
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("parameters"), &params)...)
	if resp.Diagnostics.HasError() || params.IsNull() || params.IsUnknown() {
		return
	}
	seen := map[string]int{}
	for i, element := range params.Elements() {
		object, ok := element.(types.Object)
		if !ok || object.IsNull() || object.IsUnknown() {
			continue
		}
		id, ok := object.Attributes()["id"].(types.String)
		if !ok || id.IsNull() || id.IsUnknown() {
			continue
		}
		if first, dup := seen[id.ValueString()]; dup {
			resp.Diagnostics.AddAttributeError(path.Root("parameters").AtListIndex(i).AtName("id"), "Duplicate parameter id", fmt.Sprintf("Parameter %q is already declared at index %d. Each parameter may appear once.", id.ValueString(), first))
			continue
		}
		seen[id.ValueString()] = i
	}
}

func (r *modelAccessModelResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan modelAccessModelModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state, diags := r.write(ctx, plan, nil)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *modelAccessModelResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state modelAccessModelModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Only import leaves id null. Prior state then holds no parameters to compare against, so the
	// restrictions the team already has are recorded for the first plan to diff against.
	next, err := r.read(ctx, state, state.ID.IsNull())
	if err != nil {
		if modelAccessEntryGone(ctx, r.client, err, &resp.Diagnostics) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read model access model", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

func (r *modelAccessModelResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior modelAccessModelModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state, diags := r.write(ctx, plan, prior.Parameters)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *modelAccessModelResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state modelAccessModelModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	payload := map[string]any{"enabled": false}
	if cleared := clearedParameters(state.Parameters, nil); len(cleared) > 0 {
		payload["parameters"] = cleared
	}
	if _, err := r.client.putModelAccessModel(ctx, state.ProviderID.ValueString(), state.ModelID.ValueString(), payload); err != nil && !modelAccessAlreadyReset(ctx, r.client, err) {
		resp.Diagnostics.AddError("Failed to disable model access model", err.Error())
	}
}

func (r *modelAccessModelResource) write(ctx context.Context, plan modelAccessModelModel, prior []modelParameterModel) (modelAccessModelModel, diag.Diagnostics) {
	payload, diags := modelWritePayload(ctx, plan, prior)
	if diags.HasError() {
		return plan, diags
	}
	if _, err := r.client.putModelAccessModel(ctx, plan.ProviderID.ValueString(), plan.ModelID.ValueString(), payload); err != nil {
		diags.AddError("Failed to update model access model", err.Error())
		return plan, diags
	}
	next, err := r.read(ctx, plan, false)
	if err != nil {
		diags.AddError("Failed to read model access model", err.Error())
		return plan, diags
	}
	if next.Enabled.ValueBool() != plan.Enabled.ValueBool() {
		diags.AddError("Failed to update model access model", fmt.Sprintf("The API reports enabled = %t for model %q after writing %t.", next.Enabled.ValueBool(), next.ID.ValueString(), plan.Enabled.ValueBool()))
	}
	return next, diags
}

func (r *modelAccessModelResource) read(ctx context.Context, model modelAccessModelModel, adoptParameters bool) (modelAccessModelModel, error) {
	models, err := r.client.listModelAccessModels(ctx, model.ProviderID.ValueString())
	if err != nil {
		return model, err
	}
	for _, item := range models {
		if item.ID != model.ModelID.ValueString() {
			continue
		}
		model.ID = types.StringValue(model.ProviderID.ValueString() + "/" + item.ID)
		model.DisplayName = types.StringValue(item.DisplayName)
		model.Enabled = types.BoolValue(item.Enabled)
		if model.Parameters != nil {
			model.Parameters = parametersFromAPI(ctx, model.Parameters, item.Parameters)
		} else if adoptParameters {
			model.Parameters = restrictedParameters(ctx, item.Parameters)
		}
		return model, nil
	}
	return model, &adminStatusError{API: "Team Admin API", Status: http.StatusNotFound, Message: "model was not found"}
}

// modelWritePayload sends every planned parameter and clears the ones prior state managed that the plan dropped.
func modelWritePayload(ctx context.Context, model modelAccessModelModel, prior []modelParameterModel) (map[string]any, diag.Diagnostics) {
	payload := map[string]any{"enabled": model.Enabled.ValueBool()}
	var diags diag.Diagnostics
	params := clearedParameters(prior, model.Parameters)
	for _, param := range model.Parameters {
		entry := map[string]any{}
		if param.AllowedValues.IsNull() || param.AllowedValues.IsUnknown() {
			entry["allowedValues"] = nil
		} else {
			var values []string
			diags.Append(param.AllowedValues.ElementsAs(ctx, &values, false)...)
			entry["allowedValues"] = values
		}
		if param.DefaultValue.IsNull() || param.DefaultValue.IsUnknown() {
			entry["defaultValue"] = nil
		} else {
			entry["defaultValue"] = param.DefaultValue.ValueString()
		}
		params[param.ID.ValueString()] = entry
	}
	if len(params) > 0 {
		payload["parameters"] = params
	}
	return payload, diags
}

func clearedParameters(managed, keep []modelParameterModel) map[string]any {
	kept := map[string]struct{}{}
	for _, param := range keep {
		kept[param.ID.ValueString()] = struct{}{}
	}
	out := map[string]any{}
	for _, param := range managed {
		if _, ok := kept[param.ID.ValueString()]; !ok {
			out[param.ID.ValueString()] = map[string]any{"allowedValues": nil, "defaultValue": nil}
		}
	}
	return out
}

func parametersFromAPI(ctx context.Context, prior []modelParameterModel, api []modelAccessParameterRecord) []modelParameterModel {
	byID := map[string]modelAccessParameterRecord{}
	for _, param := range api {
		byID[param.ID] = param
	}
	out := make([]modelParameterModel, len(prior))
	for i, param := range prior {
		out[i] = param
		found, ok := byID[param.ID.ValueString()]
		if !ok {
			continue
		}
		out[i].AllowedValues = keepEquivalentStringList(ctx, param.AllowedValues, found.AllowedValues)
		out[i].DefaultValue = stringPtrValue(found.ConfiguredDefaultValue)
	}
	return out
}

func restrictedParameters(ctx context.Context, api []modelAccessParameterRecord) []modelParameterModel {
	var out []modelParameterModel
	for _, param := range api {
		if param.AllowedValues == nil && param.ConfiguredDefaultValue == nil {
			continue
		}
		out = append(out, modelParameterModel{
			ID:            types.StringValue(param.ID),
			AllowedValues: keepEquivalentStringList(ctx, types.ListNull(types.StringType), param.AllowedValues),
			DefaultValue:  stringPtrValue(param.ConfiguredDefaultValue),
		})
	}
	return out
}

// keepEquivalentStringList keeps the configured list when the API returns the same values in another
// order, so a reordered response is not reported as drift. A nil API slice is null, not empty.
func keepEquivalentStringList(ctx context.Context, prior types.List, api []string) types.List {
	if api == nil {
		return types.ListNull(types.StringType)
	}
	if !prior.IsNull() && !prior.IsUnknown() {
		var configured []string
		if diags := prior.ElementsAs(ctx, &configured, false); !diags.HasError() && sameStrings(configured, api) {
			return prior
		}
	}
	values := make([]attr.Value, 0, len(api))
	for _, value := range api {
		values = append(values, types.StringValue(value))
	}
	return types.ListValueMust(types.StringType, values)
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left, right := slices.Clone(a), slices.Clone(b)
	slices.Sort(left)
	slices.Sort(right)
	return slices.Equal(left, right)
}

func configureClient(req resource.ConfigureRequest, diags diag.Diagnostics) (*apiClient, diag.Diagnostics) {
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

func isAdminNotFound(err error) bool {
	var apiErr *adminStatusError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}
