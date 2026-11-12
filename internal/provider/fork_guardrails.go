package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework/attr"
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
	owner = strings.TrimSpace(owner)
	for _, allowed := range g.AllowedOwners {
		if strings.EqualFold(strings.TrimSpace(allowed), owner) {
			return true
		}
	}
	return false
}

// ownerViolation returns the reason a repository, organization, or URL fails
// allowed_repo_owners, or "" when it passes. A value whose owner cannot be
// determined fails, so an unparseable spelling cannot slip past the allow-list.
func (g *guardrailsConfig) ownerViolation(kind, value string) string {
	if g == nil || !g.HasAllowedOwners || strings.TrimSpace(value) == "" {
		return ""
	}
	owner := publicapi.RepoOwner(value)
	if owner == "" {
		return fmt.Sprintf("guardrails.allowed_repo_owners cannot determine the owner of %s %q. Use owner/repo, host/owner/repo, or a full URL.", kind, value)
	}
	if g.ownerAllowed(owner) {
		return ""
	}
	return fmt.Sprintf("guardrails.allowed_repo_owners rejected %s %q (owner %q).", kind, value, owner)
}

func (g *guardrailsConfig) checkOwners(kind string, values []string) diag.Diagnostics {
	var diags diag.Diagnostics
	for _, value := range values {
		if detail := g.ownerViolation(kind, value); detail != "" {
			diags.AddError("Repository owner is not allowed", detail)
		}
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

// workflowGuardrailValues holds the inspected attributes as framework values rather
// than the resource model: the model's pointer and slice fields cannot represent an
// unknown object or list, and decoding one into them fails the plan with a
// conversion error instead of waiting for the value.
type workflowGuardrailValues struct {
	configScope          types.String
	planScope            types.String
	configModel          types.String
	configModelSelection types.Object
	planPrivateWorker    types.Object
	planGitRepo          types.String
	planTriggers         types.List
	planActions          types.List
}

func checkWorkflowGuardrails(ctx context.Context, g *guardrailsConfig, req resource.ModifyPlanRequest) diag.Diagnostics {
	var diags diag.Diagnostics
	if g == nil || req.Plan.Raw.IsNull() {
		return diags
	}
	var v workflowGuardrailValues
	diags.Append(req.Config.GetAttribute(ctx, path.Root("scope"), &v.configScope)...)
	diags.Append(req.Plan.GetAttribute(ctx, path.Root("scope"), &v.planScope)...)
	diags.Append(req.Config.GetAttribute(ctx, path.Root("model"), &v.configModel)...)
	diags.Append(req.Config.GetAttribute(ctx, path.Root("model_selection"), &v.configModelSelection)...)
	diags.Append(req.Plan.GetAttribute(ctx, path.Root("private_worker"), &v.planPrivateWorker)...)
	diags.Append(req.Plan.GetAttribute(ctx, path.Root("git_repo"), &v.planGitRepo)...)
	diags.Append(req.Plan.GetAttribute(ctx, path.Root("trigger"), &v.planTriggers)...)
	diags.Append(req.Plan.GetAttribute(ctx, path.Root("action"), &v.planActions)...)
	if diags.HasError() {
		return diags
	}

	hasState := !req.State.Raw.IsNull()
	var violations diag.Diagnostics
	if g.HasAllowedScopes {
		violations.Append(checkScopeGuardrail(g, v.configScope, v.planScope, hasState)...)
	}
	if g.RequirePoolLabel {
		violations.Append(checkPoolGuardrail(v.planPrivateWorker)...)
	}
	if g.HasAllowedModels {
		violations.Append(checkModelGuardrail(g, v.configModel, v.configModelSelection)...)
	}
	if g.HasAllowedOwners {
		violations.Append(checkOwnerGuardrail(g, v.planGitRepo, v.planTriggers)...)
	}
	if g.DenyPRApproval {
		violations.Append(checkApprovalGuardrail(v.planActions)...)
	}
	diags.Append(violations...)
	// A plan that changes nothing still fails: accepting it would let an import
	// bring a non-compliant automation under management. Destroy plans never reach
	// this point, so removing the automation stays possible.
	if violations.HasError() && hasState && req.Plan.Raw.Equal(req.State.Raw) {
		diags.AddWarning("Existing automation violates provider guardrails", "This automation already exists in the rejected form and the plan does not change it. Change the configuration so it satisfies the guardrails, or remove the resource from the configuration (destroy) or from state. Destroy plans are not checked.")
	}
	return diags
}

// Scope is optional and computed with UseStateForUnknown. On create an omitted scope
// is unknown in the plan and the server assigns user. On update an omitted scope plans
// the prior state value, which the update leaves untouched on the server.
func checkScopeGuardrail(g *guardrailsConfig, cfg, plan types.String, hasState bool) diag.Diagnostics {
	var diags diag.Diagnostics
	if cfg.IsUnknown() {
		return diags
	}
	p := path.Root("scope")
	var scope, note string
	switch {
	case !cfg.IsNull():
		scope = cfg.ValueString()
	case !plan.IsNull() && !plan.IsUnknown():
		scope, note = plan.ValueString(), " An omitted scope keeps the prior value."
	case hasState:
		diags.AddAttributeError(p, "Automation scope is not allowed", "guardrails.allowed_automation_scopes cannot determine the scope of this automation: scope is omitted and the prior state has no value. Set scope explicitly.")
		return diags
	default:
		scope, note = "user", " An omitted scope with no prior value is user."
	}
	if !g.scopeAllowed(scope) {
		diags.AddAttributeError(p, "Automation scope is not allowed", fmt.Sprintf("guardrails.allowed_automation_scopes rejected scope %q.%s", strings.TrimSpace(scope), note))
	}
	return diags
}

func (g *guardrailsConfig) scopeAllowed(scope string) bool {
	scope = strings.ToLower(strings.TrimSpace(scope))
	return slices.ContainsFunc(g.AllowedScopes, func(allowed string) bool {
		return strings.ToLower(strings.TrimSpace(allowed)) == scope
	})
}

func checkPoolGuardrail(worker types.Object) diag.Diagnostics {
	var diags diag.Diagnostics
	if worker.IsUnknown() {
		return diags
	}
	if worker.IsNull() {
		diags.AddAttributeError(path.Root("private_worker"), "Private worker pool is required", "guardrails.require_pool_label requires private_worker.labels.pool. Without private_worker the automation does not run on a private worker pool.")
		return diags
	}
	p := path.Root("private_worker").AtName("labels")
	labels, ok := worker.Attributes()["labels"].(types.Map)
	if !ok || labels.IsUnknown() {
		return diags
	}
	elements := labels.Elements()
	if _, ok := elements["repo"]; ok {
		diags.AddAttributeError(p.AtMapKey("repo"), "Private worker repo label is reserved", "Do not set labels.repo. Cursor sets the repo label from the run.")
	}
	pool, ok := elements["pool"].(types.String)
	if ok && pool.IsUnknown() {
		return diags
	}
	if !ok || pool.IsNull() || strings.TrimSpace(pool.ValueString()) == "" {
		diags.AddAttributeError(p, "Private worker pool is required", "guardrails.require_pool_label requires labels.pool to be a non-empty string. private_worker {} routes to any worker.")
	}
	return diags
}

func checkModelGuardrail(g *guardrailsConfig, model types.String, selection types.Object) diag.Diagnostics {
	var diags diag.Diagnostics
	if selection.IsUnknown() {
		return diags
	}
	if !selection.IsNull() {
		p := path.Root("model_selection").AtName("model_id")
		modelID, ok := selection.Attributes()["model_id"].(types.String)
		if !ok || modelID.IsUnknown() {
			return diags
		}
		if id := strings.TrimSpace(modelID.ValueString()); !slices.Contains(g.AllowedModels, id) {
			diags.AddAttributeError(p, "Model is not allowed", fmt.Sprintf("guardrails.allowed_models rejected model_selection.model_id %q.", id))
		}
		return diags
	}
	switch {
	case model.IsUnknown():
	case model.IsNull() || strings.TrimSpace(model.ValueString()) == "":
		diags.AddAttributeError(path.Root("model"), "Model is not allowed", "guardrails.allowed_models requires model or model_selection. The server assigns a default only after apply, so a plan-time check cannot see it.")
	default:
		if name := strings.TrimSpace(model.ValueString()); !slices.Contains(g.AllowedModels, name) {
			diags.AddAttributeError(path.Root("model"), "Model is not allowed", fmt.Sprintf("guardrails.allowed_models rejected model %q.", name))
		}
	}
	return diags
}

// Known owners are checked even when other values are still unknown. Unknown values
// are checked when Terraform plans again during apply, where everything is known.
func checkOwnerGuardrail(g *guardrailsConfig, gitRepo types.String, triggers types.List) diag.Diagnostics {
	var diags diag.Diagnostics
	checkString := func(p path.Path, value attr.Value) {
		s, ok := value.(types.String)
		if !ok || s.IsNull() || s.IsUnknown() {
			return
		}
		if detail := g.ownerViolation("automation repository", s.ValueString()); detail != "" {
			diags.AddAttributeError(p, "Repository owner is not allowed", detail)
		}
	}
	checkList := func(p path.Path, value attr.Value) {
		list, ok := value.(types.List)
		if !ok || list.IsNull() || list.IsUnknown() {
			return
		}
		for i, element := range list.Elements() {
			checkString(p.AtListIndex(i), element)
		}
	}
	checkString(path.Root("git_repo"), gitRepo)
	for i, trigger := range knownElements(triggers) {
		p := path.Root("trigger").AtListIndex(i)
		attrs := knownAttributes(trigger)
		if pr := knownAttributes(attrs["git_pull_request"]); pr != nil {
			checkList(p.AtName("git_pull_request").AtName("repos"), pr["repos"])
			checkList(p.AtName("git_pull_request").AtName("orgs"), pr["orgs"])
		}
		if push := knownAttributes(attrs["git_push"]); push != nil {
			checkString(p.AtName("git_push").AtName("repo"), push["repo"])
		}
		if ci := knownAttributes(attrs["git_ci_completed"]); ci != nil {
			checkList(p.AtName("git_ci_completed").AtName("repos"), ci["repos"])
		}
		if label := knownAttributes(attrs["git_label"]); label != nil {
			checkList(p.AtName("git_label").AtName("repos"), label["repos"])
		}
	}
	return diags
}

// The planned actions are inspected, not the configured ones: allow_approve is
// optional and computed, so a plan that changes nothing carries a prior true from
// state even when the configuration omits the attribute.
func checkApprovalGuardrail(actions types.List) diag.Diagnostics {
	var diags diag.Diagnostics
	for i, action := range knownElements(actions) {
		p := path.Root("action").AtListIndex(i)
		attrs := knownAttributes(action)
		if knownAttributes(attrs["approve_pr"]) != nil {
			diags.AddAttributeError(p.AtName("approve_pr"), "Pull request approval is not allowed", "guardrails.deny_pr_approval rejected action.approve_pr. approve_pr is deprecated.")
		}
		comment := knownAttributes(attrs["pr_comment"])
		if comment == nil {
			continue
		}
		if allow, ok := comment["allow_approve"].(types.Bool); ok && !allow.IsUnknown() && !allow.IsNull() && allow.ValueBool() {
			diags.AddAttributeError(p.AtName("pr_comment").AtName("allow_approve"), "Pull request approval is not allowed", "guardrails.deny_pr_approval rejected pr_comment.allow_approve. allow_approve is the switch the server honors.")
		}
	}
	return diags
}

func knownElements(list types.List) []attr.Value {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	return list.Elements()
}

// knownAttributes returns nil for anything but a known, non-null object.
func knownAttributes(value attr.Value) map[string]attr.Value {
	obj, ok := value.(types.Object)
	if !ok || obj.IsNull() || obj.IsUnknown() {
		return nil
	}
	return obj.Attributes()
}
