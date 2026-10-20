package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type guardrailsConfig struct {
	AllowedScopes    []string
	HasAllowedScopes bool
	RequirePoolLabel bool
	AllowedModels    []string
	HasAllowedModels bool
	AllowedOwners    []string
	HasAllowedOwners bool
	DenyPRApproval   bool
}

type guardrailsModel struct {
	AllowedAutomationScopes types.List `tfsdk:"allowed_automation_scopes"`
	RequirePoolLabel        types.Bool `tfsdk:"require_pool_label"`
	AllowedModels           types.List `tfsdk:"allowed_models"`
	AllowedRepoOwners       types.List `tfsdk:"allowed_repo_owners"`
	DenyPRApproval          types.Bool `tfsdk:"deny_pr_approval"`
}

func guardrailsFromConfig(ctx context.Context, model *guardrailsModel) (*guardrailsConfig, diag.Diagnostics) {
	var diags diag.Diagnostics
	if model == nil {
		return nil, diags
	}
	cfg := &guardrailsConfig{}
	if model.RequirePoolLabel.IsUnknown() || model.DenyPRApproval.IsUnknown() || model.AllowedAutomationScopes.IsUnknown() || model.AllowedModels.IsUnknown() || model.AllowedRepoOwners.IsUnknown() {
		diags.AddError("Unknown guardrails", "guardrails values must be known when the provider is configured.")
		return nil, diags
	}
	if !model.RequirePoolLabel.IsNull() {
		cfg.RequirePoolLabel = model.RequirePoolLabel.ValueBool()
	}
	if !model.DenyPRApproval.IsNull() {
		cfg.DenyPRApproval = model.DenyPRApproval.ValueBool()
	}
	if !model.AllowedAutomationScopes.IsNull() {
		cfg.HasAllowedScopes = true
		diags.Append(model.AllowedAutomationScopes.ElementsAs(ctx, &cfg.AllowedScopes, false)...)
	}
	if !model.AllowedModels.IsNull() {
		cfg.HasAllowedModels = true
		diags.Append(model.AllowedModels.ElementsAs(ctx, &cfg.AllowedModels, false)...)
	}
	if !model.AllowedRepoOwners.IsNull() {
		cfg.HasAllowedOwners = true
		diags.Append(model.AllowedRepoOwners.ElementsAs(ctx, &cfg.AllowedOwners, false)...)
	}
	return cfg, diags
}

func (g *guardrailsConfig) ownerAllowed(owner string) bool {
	if g == nil || !g.HasAllowedOwners {
		return true
	}
	for _, allowed := range g.AllowedOwners {
		if strings.EqualFold(allowed, owner) {
			return true
		}
	}
	return false
}

func (g *guardrailsConfig) checkOwners(kind string, values []string) diag.Diagnostics {
	var diags diag.Diagnostics
	if g == nil || !g.HasAllowedOwners {
		return diags
	}
	for _, value := range values {
		owner := publicapi.RepoOwner(value)
		if owner == "" || g.ownerAllowed(owner) {
			continue
		}
		diags.AddError("Repository owner is not allowed", fmt.Sprintf("guardrails.allowed_repo_owners rejected %s %q (owner %q).", kind, value, owner))
	}
	return diags
}

var _ resource.ResourceWithModifyPlan = (*platformWorkflowResource)(nil)

func (r *platformWorkflowResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || r.client == nil || r.client.guardrails == nil {
		return
	}
	resp.Diagnostics.Append(checkWorkflowGuardrails(ctx, r.client.guardrails, req)...)
}

func checkWorkflowGuardrails(ctx context.Context, g *guardrailsConfig, req resource.ModifyPlanRequest) diag.Diagnostics {
	var diags diag.Diagnostics
	if g == nil {
		return diags
	}
	var plan platformWorkflowModel
	diags.Append(req.Plan.Get(ctx, &plan)...)
	if diags.HasError() {
		return diags
	}
	var cfg platformWorkflowModel
	diags.Append(req.Config.Get(ctx, &cfg)...)
	if diags.HasError() {
		return diags
	}

	if g.HasAllowedScopes {
		diags.Append(checkScopeGuardrail(g, cfg)...)
	}
	if g.RequirePoolLabel {
		diags.Append(checkPoolGuardrail(ctx, plan, req)...)
	}
	if g.HasAllowedModels {
		diags.Append(checkModelGuardrail(g, cfg)...)
	}
	if g.HasAllowedOwners {
		if owners, unknown := workflowOwners(ctx, plan); !unknown {
			diags.Append(g.checkOwners("automation repository", owners)...)
		}
	}
	if g.DenyPRApproval && !actionsUnknown(ctx, req) {
		diags.Append(checkApprovalGuardrail(cfg)...)
	}
	return diags
}

// Scope is Optional and Computed, so an unset scope is unknown in the plan on create.
// The configuration keeps it null, which is the user scope.
func checkScopeGuardrail(g *guardrailsConfig, cfg platformWorkflowModel) diag.Diagnostics {
	var diags diag.Diagnostics
	if cfg.Scope.IsUnknown() {
		return diags
	}
	scope := "user"
	if !cfg.Scope.IsNull() && strings.TrimSpace(cfg.Scope.ValueString()) != "" {
		scope = strings.TrimSpace(cfg.Scope.ValueString())
	}
	if !slices.Contains(g.AllowedScopes, scope) {
		diags.AddAttributeError(path.Root("scope"), "Automation scope is not allowed", fmt.Sprintf("guardrails.allowed_automation_scopes rejected scope %q. An unset scope is user.", scope))
	}
	return diags
}

func checkPoolGuardrail(ctx context.Context, plan platformWorkflowModel, req resource.ModifyPlanRequest) diag.Diagnostics {
	var diags diag.Diagnostics
	if privateWorkerUnknown(plan.PrivateWorker, req, ctx) {
		return diags
	}
	if plan.PrivateWorker == nil {
		diags.AddAttributeError(path.Root("private_worker"), "Private worker pool is required", "guardrails.require_pool_label requires private_worker.labels.pool. An empty private_worker block routes to any worker.")
		return diags
	}
	if plan.PrivateWorker.Labels.IsUnknown() {
		return diags
	}
	labels := map[string]string{}
	if !plan.PrivateWorker.Labels.IsNull() {
		elemDiags := plan.PrivateWorker.Labels.ElementsAs(ctx, &labels, false)
		diags.Append(elemDiags...)
		if elemDiags.HasError() {
			return diags
		}
	}
	if strings.TrimSpace(labels["pool"]) == "" {
		diags.AddAttributeError(path.Root("private_worker").AtName("labels"), "Private worker pool is required", "guardrails.require_pool_label requires labels.pool to be a non-empty string. private_worker {} routes to any worker.")
	}
	if _, ok := labels["repo"]; ok {
		diags.AddAttributeError(path.Root("private_worker").AtName("labels"), "Private worker repo label is reserved", "Do not set labels.repo. Cursor sets the repo label from the run.")
	}
	return diags
}

func checkModelGuardrail(g *guardrailsConfig, cfg platformWorkflowModel) diag.Diagnostics {
	var diags diag.Diagnostics
	switch {
	case cfg.ModelSelection != nil:
		if cfg.ModelSelection.ModelID.IsUnknown() {
			return diags
		}
		modelID := strings.TrimSpace(cfg.ModelSelection.ModelID.ValueString())
		if !slices.Contains(g.AllowedModels, modelID) {
			diags.AddAttributeError(path.Root("model_selection").AtName("model_id"), "Model is not allowed", fmt.Sprintf("guardrails.allowed_models rejected model_selection.model_id %q.", modelID))
		}
	case cfg.Model.IsUnknown():
	case cfg.Model.IsNull() || strings.TrimSpace(cfg.Model.ValueString()) == "":
		diags.AddAttributeError(path.Root("model"), "Model is not allowed", "guardrails.allowed_models requires model or model_selection. The server assigns a default only after apply, so a plan-time check cannot see it.")
	default:
		if model := strings.TrimSpace(cfg.Model.ValueString()); !slices.Contains(g.AllowedModels, model) {
			diags.AddAttributeError(path.Root("model"), "Model is not allowed", fmt.Sprintf("guardrails.allowed_models rejected model %q.", model))
		}
	}
	return diags
}

func checkApprovalGuardrail(cfg platformWorkflowModel) diag.Diagnostics {
	var diags diag.Diagnostics
	for i, action := range cfg.Actions {
		if action.ApprovePr != nil {
			diags.AddAttributeError(path.Root("action").AtListIndex(i).AtName("approve_pr"), "Pull request approval is not allowed", "guardrails.deny_pr_approval rejected action.approve_pr. approve_pr is deprecated.")
		}
		if action.PrComment == nil {
			continue
		}
		allow := action.PrComment.AllowApprove
		if !allow.IsUnknown() && !allow.IsNull() && allow.ValueBool() {
			diags.AddAttributeError(path.Root("action").AtListIndex(i).AtName("pr_comment").AtName("allow_approve"), "Pull request approval is not allowed", "guardrails.deny_pr_approval rejected pr_comment.allow_approve. allow_approve is the switch the server honors.")
		}
	}
	return diags
}

func privateWorkerUnknown(worker *privateWorkerModel, req resource.ModifyPlanRequest, ctx context.Context) bool {
	if worker != nil {
		return false
	}
	var obj types.Object
	diags := req.Plan.GetAttribute(ctx, path.Root("private_worker"), &obj)
	return !diags.HasError() && obj.IsUnknown()
}

func actionsUnknown(ctx context.Context, req resource.ModifyPlanRequest) bool {
	var list types.List
	diags := req.Config.GetAttribute(ctx, path.Root("action"), &list)
	return !diags.HasError() && list.IsUnknown()
}

func workflowOwners(ctx context.Context, plan platformWorkflowModel) ([]string, bool) {
	var owners []string
	add := func(list types.List) bool {
		if list.IsUnknown() {
			return true
		}
		if list.IsNull() {
			return false
		}
		var values []string
		if diags := list.ElementsAs(ctx, &values, false); diags.HasError() {
			return true
		}
		owners = append(owners, values...)
		return false
	}
	addString := func(value types.String) bool {
		if value.IsUnknown() {
			return true
		}
		if !value.IsNull() && strings.TrimSpace(value.ValueString()) != "" {
			owners = append(owners, value.ValueString())
		}
		return false
	}
	if addString(plan.GitRepo) {
		return nil, true
	}
	for _, trigger := range plan.Triggers {
		if trigger.GitPullRequest != nil {
			if add(trigger.GitPullRequest.Repos) || add(trigger.GitPullRequest.Orgs) {
				return nil, true
			}
		}
		if trigger.GitPush != nil && addString(trigger.GitPush.Repo) {
			return nil, true
		}
		if trigger.GitCICompleted != nil && add(trigger.GitCICompleted.Repos) {
			return nil, true
		}
		if trigger.GitLabel != nil && add(trigger.GitLabel.Repos) {
			return nil, true
		}
	}
	return owners, false
}
