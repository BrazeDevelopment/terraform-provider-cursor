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

// The configuration is a team singleton; any import id reads the policy for the key's team.
func (r *modelAccessConfigurationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *modelAccessProviderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("provider_id"), req, resp)
}

func (r *modelAccessModelResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	providerID, modelID, ok := strings.Cut(strings.TrimSpace(req.ID), "/")
	if !ok || providerID == "" || modelID == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Import a model as <provider_id>/<model_id>, for example anthropic/claude-opus-4-6.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("provider_id"), providerID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("model_id"), modelID)...)
}

func (r *userSpendLimitResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("user_email"), req, resp)
}

// Import by the repo_ id from the blocklist read.
func (r *repoBlocklistResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *groupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *groupMembersResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("group_id"), req, resp)
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
