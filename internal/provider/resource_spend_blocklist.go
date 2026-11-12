package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var userEmailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+$`)

const userEmailPatternMessage = "must be an email address without whitespace"

type userSpendLimitResource struct{ client *apiClient }

type userSpendLimitModel struct {
	ID                types.String `tfsdk:"id"`
	UserEmail         types.String `tfsdk:"user_email"`
	SpendLimitDollars types.Int64  `tfsdk:"spend_limit_dollars"`
}

func NewUserSpendLimitResource() resource.Resource { return &userSpendLimitResource{} }

func (r *userSpendLimitResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_spend_limit"
}

func (r *userSpendLimitResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Sets one team member's monthly spend limit. spend_limit_dollars is an integer. 0 sets a zero-dollar limit. Null, including destroy, sends null and removes the limit. Create fails when the member already has a different limit; import the resource to manage an existing limit. There is no dedicated read; refresh uses monthlyLimitDollars from the paginated spend list and ignores hardLimitOverrideDollars. Refresh drops the resource from state when the member is no longer in the team's spend data. Emails are matched without regard to letter case. Import with the member email. Enterprise only. The bulk route is not used.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "The member email in lower case."},
			"user_email": schema.StringAttribute{
				Required: true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplaceIf(
					replaceUnlessSameEmail,
					"Changing the email to a different member will destroy and recreate the resource. A change of letter case alone updates in place.",
					"Changing the email to a different member will destroy and recreate the resource. A change of letter case alone updates in place.",
				)},
				Validators:  []validator.String{stringvalidator.RegexMatches(userEmailPattern, userEmailPatternMessage)},
				Description: "Email of the team member. Changing it to a different member replaces the resource; letter case is not significant.",
			},
			"spend_limit_dollars": schema.Int64Attribute{
				Optional:    true,
				Validators:  []validator.Int64{int64validator.AtLeast(0)},
				Description: "Integer spend limit in dollars, 0 or more. 0 is a zero-dollar limit. Null removes the limit.",
			},
		},
	}
}

func (r *userSpendLimitResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client, resp.Diagnostics = configureClient(req, resp.Diagnostics)
}

func (r *userSpendLimitResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userSpendLimitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	email := plan.UserEmail.ValueString()
	want := int64Ptr(plan.SpendLimitDollars)
	existing, found, err := r.client.findUserSpend(ctx, email)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read user spend limit", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddError("Team member not found", fmt.Sprintf("%s is not in the team's spend data, so no limit was set.", email))
		return
	}
	if existing != nil && (want == nil || *want != *existing) {
		resp.Diagnostics.AddError("User spend limit already exists", fmt.Sprintf("%s already has a spend limit of %d dollars that Terraform does not manage. Import it with `terraform import <address> %s`, or remove the limit first.", email, *existing, email))
		return
	}
	state, err := r.write(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to set user spend limit", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userSpendLimitResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userSpendLimitModel
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
		resp.Diagnostics.AddError("Failed to read user spend limit", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

func (r *userSpendLimitResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan userSpendLimitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state, err := r.write(ctx, plan)
	if err != nil {
		resp.Diagnostics.AddError("Failed to set user spend limit", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userSpendLimitResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userSpendLimitModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	email := state.UserEmail.ValueString()
	err := r.client.setUserSpendLimit(ctx, email, nil)
	if err == nil || isAdminNotFound(err) {
		return
	}
	// The route has no stable not-found status, so a failed clear is checked against the spend
	// list: a member who left the team, or who has no limit, is already in the destroyed state.
	if limit, found, findErr := r.client.findUserSpend(ctx, email); findErr == nil && (!found || limit == nil) {
		return
	}
	resp.Diagnostics.AddError("Failed to clear user spend limit", err.Error())
}

func (r *userSpendLimitResource) write(ctx context.Context, plan userSpendLimitModel) (userSpendLimitModel, error) {
	want := int64Ptr(plan.SpendLimitDollars)
	if err := r.client.setUserSpendLimit(ctx, plan.UserEmail.ValueString(), want); err != nil {
		return plan, err
	}
	next, err := r.read(ctx, plan)
	if err != nil {
		return plan, fmt.Errorf("reading the limit after the write: %w", err)
	}
	got := int64Ptr(next.SpendLimitDollars)
	if (got == nil) != (want == nil) || (got != nil && *got != *want) {
		return plan, fmt.Errorf("the spend list reports a limit of %s for %s after writing %s", describeLimit(got), plan.UserEmail.ValueString(), describeLimit(want))
	}
	return next, nil
}

func (r *userSpendLimitResource) read(ctx context.Context, model userSpendLimitModel) (userSpendLimitModel, error) {
	limit, ok, err := r.client.findUserSpend(ctx, model.UserEmail.ValueString())
	if err != nil {
		return model, err
	}
	if !ok {
		return model, &adminStatusError{API: "Team Admin API", Status: http.StatusNotFound, Message: "user was not found in spend data"}
	}
	model.ID = types.StringValue(userSpendLimitID(model.UserEmail.ValueString()))
	if limit == nil {
		model.SpendLimitDollars = types.Int64Null()
	} else {
		model.SpendLimitDollars = types.Int64Value(*limit)
	}
	return model, nil
}

// The id is case-folded so that respelling user_email in place does not change it.
func userSpendLimitID(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func describeLimit(limit *int64) string {
	if limit == nil {
		return "none"
	}
	return fmt.Sprintf("%d dollars", *limit)
}

func replaceUnlessSameEmail(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	resp.RequiresReplace = req.PlanValue.IsUnknown() || req.StateValue.IsNull() || userSpendLimitID(req.StateValue.ValueString()) != userSpendLimitID(req.PlanValue.ValueString())
}

func userSpendLimitImportEmail(id string) (string, error) {
	email := strings.TrimSpace(id)
	if !userEmailPattern.MatchString(email) {
		return "", fmt.Errorf("import id %q is not an email address; import a spend limit with the member email, for example owner@example.com", id)
	}
	return email, nil
}

func int64Ptr(value types.Int64) *int64 {
	if value.IsNull() || value.IsUnknown() {
		return nil
	}
	n := value.ValueInt64()
	return &n
}

type repoBlocklistResource struct{ client *apiClient }

type repoBlocklistModel struct {
	ID       types.String `tfsdk:"id"`
	URL      types.String `tfsdk:"url"`
	Patterns types.List   `tfsdk:"patterns"`
}

func NewRepoBlocklistResource() resource.Resource { return &repoBlocklistResource{} }

func (r *repoBlocklistResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_repo_blocklist"
}

func (r *repoBlocklistResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the blocklist patterns for one repository URL. Declare each repository once. Create fails when the repository already has a blocklist entry; import the resource to manage an existing entry. Upsert replaces patterns for that repository and leaves every other repository alone. Refresh drops the resource from state when the entry is gone, and follows the entry by URL when it was recreated under a new id. Delete uses the repo_ id from the read. Import with the repo_ id or the repository URL. Blocklists apply to the editor and CLI, not to Cloud Agents or automations.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "Repository blocklist id (repo_…)."},
			"url": schema.StringAttribute{
				Required: true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplaceIf(
					replaceUnlessSameRepo,
					"Changing the URL to a different repository will destroy and recreate the resource. Another spelling of the same repository updates in place.",
					"Changing the URL to a different repository will destroy and recreate the resource. Another spelling of the same repository updates in place.",
				)},
				Validators:  []validator.String{stringvalidator.RegexMatches(repoBlocklistURLPattern, "must not be empty or contain whitespace")},
				Description: "Repository URL. Changing it to a different repository replaces the resource; scheme, letter case, and a trailing .git are not significant.",
			},
			"patterns": schema.ListAttribute{
				ElementType: types.StringType,
				Required:    true,
				Validators:  []validator.List{listvalidator.SizeAtLeast(1), listvalidator.UniqueValues(), listvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1))},
				Description: "File patterns to block, at least one. * blocks the entire repository. Order is not significant.",
			},
		},
	}
}

func (r *repoBlocklistResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client, resp.Diagnostics = configureClient(req, resp.Diagnostics)
}

func (r *repoBlocklistResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan repoBlocklistModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	repos, err := r.client.listRepoBlocklists(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read repository blocklists", err.Error())
		return
	}
	if existing := findRepoBlocklistByURL(repos, plan.URL.ValueString()); existing != nil {
		resp.Diagnostics.AddError("Repository blocklist already exists", fmt.Sprintf("%s already has blocklist entry %s that Terraform does not manage; creating would replace its patterns. Import it with `terraform import <address> %s`.", existing.URL, existing.ID, existing.ID))
		return
	}
	record, err := r.client.upsertRepoBlocklist(ctx, plan.URL.ValueString(), stringValues(ctx, plan.Patterns))
	if err != nil {
		resp.Diagnostics.AddError("Failed to upsert repository blocklist", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, blocklistModel(ctx, record, plan.URL, plan.Patterns))...)
}

func (r *repoBlocklistResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state repoBlocklistModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	repos, err := r.client.listRepoBlocklists(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read repository blocklists", err.Error())
		return
	}
	var match *repoBlocklistRecord
	if id := state.ID.ValueString(); id != "" {
		for i := range repos {
			if repos[i].ID == id {
				match = &repos[i]
				break
			}
		}
	}
	// The URL is the natural key: an entry deleted and added again gets a new id, and an import by URL has no id yet.
	if match == nil && state.URL.ValueString() != "" {
		match = findRepoBlocklistByURL(repos, state.URL.ValueString())
	}
	if match == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, blocklistModel(ctx, match, state.URL, state.Patterns))...)
}

func (r *repoBlocklistResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state repoBlocklistModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A respelled URL updates in place. The entry is addressed by the spelling already in state,
	// which the API is known to resolve to this entry, so the upsert cannot add a second one.
	target := plan.URL.ValueString()
	if prior := state.URL.ValueString(); prior != "" && sameRepo(prior, target) {
		target = prior
	}
	record, err := r.client.upsertRepoBlocklist(ctx, target, stringValues(ctx, plan.Patterns))
	if err != nil {
		resp.Diagnostics.AddError("Failed to upsert repository blocklist", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, blocklistModel(ctx, record, plan.URL, plan.Patterns))...)
}

func (r *repoBlocklistResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state repoBlocklistModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.deleteRepoBlocklist(ctx, state.ID.ValueString()); err != nil && !isAdminNotFound(err) {
		resp.Diagnostics.AddError("Failed to delete repository blocklist", err.Error())
	}
}

func blocklistModel(ctx context.Context, record *repoBlocklistRecord, priorURL types.String, priorPatterns types.List) *repoBlocklistModel {
	patterns := record.Patterns
	if patterns == nil {
		patterns = []string{}
	}
	return &repoBlocklistModel{
		ID:       types.StringValue(record.ID),
		URL:      keepEquivalentRepoURL(priorURL, record.URL),
		Patterns: keepEquivalentStringList(ctx, priorPatterns, patterns),
	}
}

var repoBlocklistURLPattern = regexp.MustCompile(`^\S+$`)

func sameRepo(a, b string) bool {
	canonical := publicapi.CanonicalRepo(a)
	return canonical != "" && canonical == publicapi.CanonicalRepo(b)
}

func findRepoBlocklistByURL(repos []repoBlocklistRecord, repoURL string) *repoBlocklistRecord {
	for i := range repos {
		if repos[i].URL == repoURL {
			return &repos[i]
		}
	}
	for i := range repos {
		if sameRepo(repos[i].URL, repoURL) {
			return &repos[i]
		}
	}
	return nil
}

func replaceUnlessSameRepo(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	resp.RequiresReplace = req.PlanValue.IsUnknown() || req.StateValue.IsNull() || !sameRepo(req.StateValue.ValueString(), req.PlanValue.ValueString())
}

// repoBlocklistImportTarget maps an import id to the attribute it fills: a repo_ id or a repository URL.
func repoBlocklistImportTarget(id string) (attribute, value string, err error) {
	value = strings.TrimSpace(id)
	switch {
	case value == "" || !repoBlocklistURLPattern.MatchString(value):
		return "", "", fmt.Errorf("import id %q is not valid; import a repository blocklist with its repo_ id or its repository URL", id)
	case strings.HasPrefix(value, "repo_"):
		return "id", value, nil
	default:
		return "url", value, nil
	}
}
