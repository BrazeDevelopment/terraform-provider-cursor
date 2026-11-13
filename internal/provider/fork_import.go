package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

var (
	_ resource.ResourceWithImportState = (*modelAccessConfigurationResource)(nil)
	_ resource.ResourceWithImportState = (*modelAccessProviderResource)(nil)
	_ resource.ResourceWithImportState = (*modelAccessModelResource)(nil)
	_ resource.ResourceWithImportState = (*userSpendLimitResource)(nil)
	_ resource.ResourceWithImportState = (*repoBlocklistResource)(nil)
	_ resource.ResourceWithImportState = (*groupResource)(nil)
	_ resource.ResourceWithImportState = (*groupMembersResource)(nil)
)

// The configuration is a team singleton; the import id is the team id or any non-numeric word such as team.
func (r *modelAccessConfigurationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := modelAccessConfigurationImportID(ctx, r.client, req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Cannot import model access configuration", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}

func (r *modelAccessProviderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	providerID := strings.TrimSpace(req.ID)
	if !modelAccessProviderIDPattern.MatchString(providerID) {
		resp.Diagnostics.AddError("Invalid import ID", "Import a provider as <provider_id>, for example anthropic. Models are imported with cursor_model_access_model.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("provider_id"), providerID)...)
}

func (r *modelAccessModelResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	providerID, modelID, ok := strings.Cut(strings.TrimSpace(req.ID), "/")
	if !ok || !modelAccessProviderIDPattern.MatchString(providerID) || !modelAccessModelIDPattern.MatchString(modelID) {
		resp.Diagnostics.AddError("Invalid import ID", "Import a model as <provider_id>/<model_id>, for example anthropic/claude-opus-4-6.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("provider_id"), providerID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("model_id"), modelID)...)
}

func (r *userSpendLimitResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	email, err := userSpendLimitImportEmail(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_email"), email)...)
}

// Import by the repo_ id from the blocklist read, or by the repository URL.
func (r *repoBlocklistResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	attribute, value, err := repoBlocklistImportTarget(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(attribute), value)...)
}

func (r *groupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := groupImportID(r.kind, req.ID, false)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}

func (r *groupMembersResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := groupImportID(r.kind, req.ID, true)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("group_id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}
