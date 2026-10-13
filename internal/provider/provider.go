package provider

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	envToken      = "CURSOR_TOKEN"
	envEndpoint   = "CURSOR_ENDPOINT"
	envTeamAPIKey = "CURSOR_TEAM_API_KEY"
	envOrgAPIKey  = "CURSOR_ORGANIZATION_API_KEY"
	envAPIKey     = "CURSOR_API_KEY"
	envPoolAPIKey = "CURSOR_POOL_API_KEY"
	envOrgID      = "CURSOR_ORGANIZATION_ID"

	defaultEndpoint = "https://api2.cursor.sh"
)

type cursorProvider struct {
	version string
}

type cursorProviderModel struct {
	Token              types.String     `tfsdk:"token"`
	Endpoint           types.String     `tfsdk:"endpoint"`
	TeamAPIKey         types.String     `tfsdk:"team_api_key"`
	OrganizationAPIKey types.String     `tfsdk:"organization_api_key"`
	APIKey             types.String     `tfsdk:"api_key"`
	OrganizationID     types.String     `tfsdk:"organization_id"`
	PoolAPIKey         types.String     `tfsdk:"pool_api_key"`
	Guardrails         *guardrailsModel `tfsdk:"guardrails"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &cursorProvider{version: version}
	}
}

func (p *cursorProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "cursor"
	resp.Version = p.version
}

func (p *cursorProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage Cursor Automations, Cloud Agent environments, secrets, private worker pools, team model access and spend controls, and Origin resources. Automations use the Cursor Automations API over Connect RPC. Cloud Agents resources use https://api.cursor.com with an API key. Requests rejected with HTTP 429 are retried after the delay in the API's Retry-After or X-RateLimit-Reset header, or with exponential backoff; GET, PUT, and DELETE requests are also retried on HTTP 502, 503, and 504. POST is retried on HTTP 503 only when the response includes Retry-After.",
		Attributes: map[string]schema.Attribute{
			"token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Auth token for the Cursor Automations and Origin APIs. Required when the configuration manages automations or Origin resources. A raw key_ or crsr_ value is exchanged for a session token. Can also be set via CURSOR_TOKEN. Optional when api_key, team_api_key, or pool_api_key is set and the stack does not manage automations.",
			},
			"endpoint": schema.StringAttribute{
				Optional:    true,
				Description: fmt.Sprintf("Cursor Automations API base URL. Defaults to %s. Can also be set via CURSOR_ENDPOINT.", defaultEndpoint),
			},
			"api_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Raw API key for the Cloud Agents API (https://api.cursor.com), sent as HTTP Basic auth. Defaults to token when token is still a raw key_ or crsr_ key, captured before that key is exchanged for a session token. Can also be set via CURSOR_API_KEY.",
			},
			"team_api_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: fmt.Sprintf("Team Admin API key, sent as HTTP Basic auth (key as username, empty password) to %s. Used for Origin grants keyed by user_email and for Admin API resources. Model-access writes need models:* or admin:*. Can also be set via %s.", defaultAdminAPIBase, envTeamAPIKey),
			},
			"organization_api_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: fmt.Sprintf("Organization Admin API key, sent as HTTP Basic auth (key as username, empty password) to %s. One key serves Origin group_name lookup and Organization API resources. Use the narrowest scope that covers every resource in the stack. Can also be set via %s.", defaultAdminAPIBase, envOrgAPIKey),
			},
			"organization_id": schema.StringAttribute{
				Optional:    true,
				Description: "Public organization ID (org_…). Caller-supplied; it does not prove the organization API key belongs to that organization. Required by Organization API resources. Can also be set via CURSOR_ORGANIZATION_ID.",
			},
			"pool_api_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Team service-account key for private worker pool resources. The pools API rejects other key types. Defaults to api_key. One key is used for every pool in the team. Can also be set via CURSOR_POOL_API_KEY.",
			},
		},
		Blocks: map[string]schema.Block{
			"guardrails": schema.SingleNestedBlock{
				Description: "Plan-time checks for automations and related resources. Violations fail the plan. Unknown values wait until they are known. This does not constrain launches made from the dashboard or API outside this provider.",
				Attributes: map[string]schema.Attribute{
					"allowed_automation_scopes": schema.ListAttribute{
						ElementType: types.StringType,
						Optional:    true,
						Description: "Automation scopes this provider may manage. An unset scope is user. team and team_editable both run as the team service account and are different scopes. team_visible and team_editable_user run as the creator.",
					},
					"require_pool_label": schema.BoolAttribute{
						Optional:    true,
						Description: "Require private_worker.labels.pool to be a non-empty string. An empty private_worker block routes to any worker and fails this rule. A configured repo label also fails; Cursor sets repo itself.",
					},
					"allowed_models": schema.ListAttribute{
						ElementType: types.StringType,
						Optional:    true,
						Description: "Automation catalog model ids. When model_selection is set, model_selection.model_id is compared. Otherwise model is compared. When both are unset, the plan fails because the server assigns a default only after apply.",
					},
					"allowed_repo_owners": schema.ListAttribute{
						ElementType: types.StringType,
						Optional:    true,
						Description: "Repository owners allowed on automation triggers, git_repo, environment repos, and repo-backed pools. Compared against the owner segment of org/repo, github.com/org/repo, and full URLs.",
					},
					"deny_pr_approval": schema.BoolAttribute{
						Optional:    true,
						Description: "Reject action.approve_pr and action.pr_comment.allow_approve = true. approve_pr is deprecated. allow_approve is the switch the server honors.",
					},
				},
			},
		},
	}
}

func (p *cursorProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config cursorProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	token := getStringValue(config.Token, envToken)
	teamKey := getStringValue(config.TeamAPIKey, envTeamAPIKey)
	orgKey := getStringValue(config.OrganizationAPIKey, envOrgAPIKey)
	orgID := getStringValue(config.OrganizationID, envOrgID)
	apiKey, poolKey := resolveForkKeys(token, getStringValue(config.APIKey, envAPIKey), getStringValue(config.PoolAPIKey, envPoolAPIKey))
	if token == "" && apiKey == "" && teamKey == "" && poolKey == "" {
		resp.Diagnostics.AddError(
			"Missing Cursor API token",
			fmt.Sprintf("Set the provider \"token\" attribute or the %s environment variable. token is optional when api_key, team_api_key, or pool_api_key is set and the stack does not manage automations.", envToken),
		)
		return
	}

	endpoint := getStringValue(config.Endpoint, envEndpoint)
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	endpoint = strings.TrimRight(endpoint, "/")

	client, err := newAPIClient(ctx, endpoint, token, p.version, adminKeys{
		team:         teamKey,
		organization: orgKey,
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to configure Cursor client", err.Error())
		return
	}
	client.apiKey = apiKey
	client.poolAPIKey = poolKey
	client.organizationID = orgID
	if config.Guardrails != nil {
		guardrails, diags := guardrailsFromConfig(ctx, config.Guardrails)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		client.guardrails = guardrails
	}
	agent := userAgentPrefix + strings.TrimSpace(p.version) + "+braze"
	if apiKey != "" {
		client.public = publicapi.New(publicapi.Options{
			BaseURL:   defaultAdminAPIBase,
			APIKey:    apiKey,
			UserAgent: agent,
			HTTP:      client.httpClient,
		})
	}
	if poolKey != "" {
		if client.public != nil && poolKey == apiKey {
			client.pools = client.public
		} else {
			client.pools = publicapi.New(publicapi.Options{
				BaseURL:   defaultAdminAPIBase,
				APIKey:    poolKey,
				UserAgent: agent,
				HTTP:      client.httpClient,
			})
		}
	}

	resp.DataSourceData = client
	resp.ResourceData = client
}

func (p *cursorProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewPlatformWorkflowResource,
		NewOriginRepoRulesetResource,
		NewOriginRepoGrantResource,
		NewOriginOwnerGrantResource,
		NewOriginSSHCertificateAuthorityResource,
		NewOriginSSHCertificateRequirementResource,
		NewOriginInboundIPAllowlistResource,
		NewOriginInboundIPAllowlistEntryResource,
		NewOriginInboundIPAllowlistEntriesResource,
		NewEnvironmentResource,
		NewEnvironmentSecretResource,
		NewTeamSecretResource,
		NewPrivateWorkerPoolResource,
		NewModelAccessConfigurationResource,
		NewModelAccessProviderResource,
		NewModelAccessModelResource,
		NewUserSpendLimitResource,
		NewTeamDirectoryGroupResource,
		NewTeamDirectoryGroupMembersResource,
		NewOrganizationGroupResource,
		NewOrganizationGroupMembersResource,
		NewRepoBlocklistResource,
	}
}

func (p *cursorProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewPlatformWorkflowDataSource,
		NewOriginRepoDataSource,
		NewOriginRepoGrantsDataSource,
		NewOriginOwnerGrantsDataSource,
		NewOriginSSHCertificateAuthoritiesDataSource,
		NewOriginInboundIPAllowlistEntriesDataSource,
		NewAPIKeyInfoDataSource,
		NewTeamMembersDataSource,
		NewEnvironmentDataSource,
		NewEnvironmentsDataSource,
		NewEnvironmentActiveBuildDataSource,
		NewModelsDataSource,
		NewPrivateWorkerPoolsDataSource,
		NewPrivateWorkersSummaryDataSource,
	}
}

// resolveForkKeys captures a raw key_ or crsr_ token before session exchange
// and defaults the pool key to that API key.
func resolveForkKeys(token, apiKey, poolKey string) (string, string) {
	if apiKey == "" && isAPIKey(token) {
		apiKey = strings.TrimSpace(token)
	}
	if poolKey == "" && apiKey != "" {
		poolKey = apiKey
	}
	return apiKey, poolKey
}

func getStringValue(value types.String, envKey string) string {
	if !value.IsNull() && !value.IsUnknown() {
		return strings.TrimSpace(value.ValueString())
	}
	return strings.TrimSpace(os.Getenv(envKey))
}
