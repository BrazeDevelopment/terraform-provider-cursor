package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
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
		Description: "Manages the team's model-access policy. The first apply turns the policy on and seeds the catalog. Provider and model resources return 409 until this resource has been applied. Destroying it sends state unrestricted, which clears the custom policy. deletion_protection defaults to true. Organization Groups can still allow a model the team policy disables; this API is the team baseline. These ids are Admin catalog ids, a different space from automation model slugs.",
		Attributes: map[string]schema.Attribute{
			"id":                   schema.StringAttribute{Computed: true, Description: "Team id as a string."},
			"team_id":              schema.Int64Attribute{Computed: true, Description: "Team id implied by the API key."},
			"state":                schema.StringAttribute{Computed: true, Description: "unrestricted, custom, or legacy."},
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
	out, err := r.client.putModelAccessConfiguration(ctx, map[string]string{
		"newProviderDefault": plan.NewProviderDefault.ValueString(),
		"newModelDefault":    plan.NewModelDefault.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to update model access configuration", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, configurationModel(plan, out))...)
}

func (r *modelAccessConfigurationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state modelAccessConfigurationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.getModelAccessConfiguration(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read model access configuration", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, configurationModel(state, out))...)
}

func (r *modelAccessConfigurationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan modelAccessConfigurationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, err := r.client.putModelAccessConfiguration(ctx, map[string]string{
		"newProviderDefault": plan.NewProviderDefault.ValueString(),
		"newModelDefault":    plan.NewModelDefault.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to update model access configuration", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, configurationModel(plan, out))...)
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
	if _, err := r.client.putModelAccessConfiguration(ctx, map[string]string{"state": "unrestricted"}); err != nil {
		resp.Diagnostics.AddError("Failed to clear model access configuration", err.Error())
	}
}

func configurationModel(prior modelAccessConfigurationModel, out *modelAccessConfiguration) *modelAccessConfigurationModel {
	prior.TeamID = types.Int64Value(out.TeamID)
	prior.ID = types.StringValue(fmt.Sprintf("%d", out.TeamID))
	prior.State = types.StringValue(out.State)
	prior.NewProviderDefault = stringPtrValue(out.NewProviderDefault)
	prior.NewModelDefault = stringPtrValue(out.NewModelDefault)
	prior.DeletionProtection = boolOrDefault(prior.DeletionProtection, true)
	return &prior
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
		Description: "Enables or disables one model-access provider. Catalog ids look like anthropic. Reads and writes return 409 while configuration state is unrestricted or legacy; apply cursor_model_access_configuration first.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, Description: "Provider catalog id."},
			"provider_id":  schema.StringAttribute{Required: true, Description: "Catalog provider id, such as anthropic.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
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
	if err := r.client.putModelAccessProvider(ctx, plan.ProviderID.ValueString(), plan.Enabled.ValueBool()); err != nil {
		resp.Diagnostics.AddError("Failed to update model access provider", err.Error())
		return
	}
	state, err := r.read(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read model access provider", err.Error())
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
		if isAdminNotFound(err) {
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
	if err := r.client.putModelAccessProvider(ctx, plan.ProviderID.ValueString(), plan.Enabled.ValueBool()); err != nil {
		resp.Diagnostics.AddError("Failed to update model access provider", err.Error())
		return
	}
	state, err := r.read(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read model access provider", err.Error())
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
	if err := r.client.putModelAccessProvider(ctx, state.ProviderID.ValueString(), false); err != nil && !isAdminNotFound(err) {
		resp.Diagnostics.AddError("Failed to disable model access provider", err.Error())
	}
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
	return model, &adminStatusError{API: "Team Admin API", Status: 404, Message: "provider was not found"}
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
		Description: "Enables or disables one model and, optionally, its parameter restrictions. Catalog ids look like claude-opus-4-6. Reads and writes return 409 until cursor_model_access_configuration has been applied. When parameters is omitted, refresh does not record the catalog parameters.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Computed: true, Description: "provider/model."},
			"provider_id":  schema.StringAttribute{Required: true, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, Description: "Catalog provider id."},
			"model_id":     schema.StringAttribute{Required: true, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}, Description: "Catalog model id."},
			"display_name": schema.StringAttribute{Computed: true, Description: "Display name returned by the API."},
			"enabled":      schema.BoolAttribute{Required: true, Description: "Whether the model is enabled."},
			"parameters": schema.ListNestedAttribute{
				Optional:    true,
				Description: "Per-parameter allowed values and default. Omitted parameters are left unchanged. Null allowed_values clears a restriction.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id":             schema.StringAttribute{Required: true, Description: "Parameter id, such as fast or reasoning."},
					"allowed_values": schema.ListAttribute{ElementType: types.StringType, Optional: true, Description: "Values members may pick. Null clears the restriction."},
					"default_value":  schema.StringAttribute{Optional: true, Description: "Team default. Null restores the catalog default."},
				}},
			},
		},
	}
}

func (r *modelAccessModelResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client, resp.Diagnostics = configureClient(req, resp.Diagnostics)
}

func (r *modelAccessModelResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan modelAccessModelModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	payload, diags := modelWritePayload(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.client.putModelAccessModel(ctx, plan.ProviderID.ValueString(), plan.ModelID.ValueString(), payload); err != nil {
		resp.Diagnostics.AddError("Failed to update model access model", err.Error())
		return
	}
	state, err := r.read(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read model access model", err.Error())
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
	next, err := r.read(ctx, state)
	if err != nil {
		if isAdminNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read model access model", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

func (r *modelAccessModelResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan modelAccessModelModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	payload, diags := modelWritePayload(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.client.putModelAccessModel(ctx, plan.ProviderID.ValueString(), plan.ModelID.ValueString(), payload); err != nil {
		resp.Diagnostics.AddError("Failed to update model access model", err.Error())
		return
	}
	state, err := r.read(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read model access model", err.Error())
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
	if _, err := r.client.putModelAccessModel(ctx, state.ProviderID.ValueString(), state.ModelID.ValueString(), map[string]any{"enabled": false}); err != nil && !isAdminNotFound(err) {
		resp.Diagnostics.AddError("Failed to disable model access model", err.Error())
	}
}

func (r *modelAccessModelResource) read(ctx context.Context, model modelAccessModelModel) (modelAccessModelModel, error) {
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
		}
		return model, nil
	}
	return model, &adminStatusError{API: "Team Admin API", Status: 404, Message: "model was not found"}
}

func modelWritePayload(ctx context.Context, model modelAccessModelModel) (map[string]any, diag.Diagnostics) {
	payload := map[string]any{"enabled": model.Enabled.ValueBool()}
	var diags diag.Diagnostics
	if len(model.Parameters) == 0 {
		return payload, diags
	}
	params := map[string]any{}
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
	payload["parameters"] = params
	return payload, diags
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
		values := found.AllowedValues
		if values == nil {
			values = []string{}
		}
		list, diags := types.ListValueFrom(ctx, types.StringType, values)
		if !diags.HasError() {
			out[i].AllowedValues = list
		}
		out[i].DefaultValue = stringPtrValue(found.ConfiguredDefaultValue)
	}
	return out
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
	apiErr, ok := err.(*adminStatusError)
	return ok && apiErr.Status == 404
}
