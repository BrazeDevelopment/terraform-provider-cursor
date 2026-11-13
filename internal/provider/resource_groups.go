package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
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

type groupKind int

const (
	groupKindTeam groupKind = iota
	groupKindOrganization
)

const maxGroupSpendingLimitDollars = 2147483647

var (
	teamGroupIDPattern         = regexp.MustCompile(`^team_group_\S+$`)
	teamGroupImportIDPattern   = regexp.MustCompile(`^(team_group_|grp_)\S+$`)
	organizationGroupIDPattern = regexp.MustCompile(`^g_\S+$`)
	groupUserIDPattern         = regexp.MustCompile(`^[^\s@]+$`)
)

func (k groupKind) noun() string {
	if k == groupKindOrganization {
		return "Organization group"
	}
	return "Team directory group"
}

func (k groupKind) owner() string {
	if k == groupKindOrganization {
		return "organization"
	}
	return "team"
}

func (k groupKind) idPattern() (*regexp.Regexp, string) {
	if k == groupKindOrganization {
		return organizationGroupIDPattern, "must be an Organization group id with the g_ prefix; the grp_ public id is not accepted"
	}
	return teamGroupIDPattern, "must be a Team directory group id with the team_group_ prefix; the grp_ public id is not accepted"
}

// groupImportID checks an import id against the id forms the routes accept. The team get route
// also takes the grp_ public id; member writes take only the team_group_ id.
func groupImportID(kind groupKind, raw string, members bool) (string, error) {
	id := strings.TrimSpace(raw)
	switch {
	case kind == groupKindOrganization:
		if !organizationGroupIDPattern.MatchString(id) {
			return "", fmt.Errorf("import id %q is not an Organization group id. Import with the group id, which uses the g_ prefix, for example g_PDSPmvukpYgZEDXsoNirw3CFhy. The grp_ public id is not accepted", raw)
		}
	case members:
		if !teamGroupIDPattern.MatchString(id) {
			return "", fmt.Errorf("import id %q is not a Team directory group id. Import with the group id, which uses the team_group_ prefix, for example team_group_01k2ja2000e0080000000000n2. Member changes do not accept the grp_ public id", raw)
		}
	default:
		if !teamGroupImportIDPattern.MatchString(id) {
			return "", fmt.Errorf("import id %q is not a Team directory group id. Import with the group id, which uses the team_group_ prefix, for example team_group_01k2ja2000e0080000000000n2, or with its grp_ public id", raw)
		}
	}
	return id, nil
}

type groupResource struct {
	client *apiClient
	kind   groupKind
}

type groupModel struct {
	ID                          types.String `tfsdk:"id"`
	Name                        types.String `tfsdk:"name"`
	PublicID                    types.String `tfsdk:"public_id"`
	MemberCount                 types.Int64  `tfsdk:"member_count"`
	MonthlySpendingLimitDollars types.Int64  `tfsdk:"monthly_spending_limit_dollars"`
}

type orgGroupModel struct {
	ID                          types.String `tfsdk:"id"`
	Name                        types.String `tfsdk:"name"`
	PublicID                    types.String `tfsdk:"public_id"`
	OrganizationID              types.String `tfsdk:"organization_id"`
	MemberCount                 types.Int64  `tfsdk:"member_count"`
	MonthlySpendingLimitDollars types.Int64  `tfsdk:"monthly_spending_limit_dollars"`
}

type groupMembersResource struct {
	client *apiClient
	kind   groupKind
}

type groupMembersModel struct {
	ID      types.String `tfsdk:"id"`
	GroupID types.String `tfsdk:"group_id"`
	UserIDs types.Set    `tfsdk:"user_ids"`
}

func NewTeamDirectoryGroupResource() resource.Resource {
	return &groupResource{kind: groupKindTeam}
}
func NewTeamDirectoryGroupMembersResource() resource.Resource {
	return &groupMembersResource{kind: groupKindTeam}
}
func NewOrganizationGroupResource() resource.Resource {
	return &groupResource{kind: groupKindOrganization}
}
func NewOrganizationGroupMembersResource() resource.Resource {
	return &groupMembersResource{kind: groupKindOrganization}
}

func (r *groupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	if r.kind == groupKindOrganization {
		resp.TypeName = req.ProviderTypeName + "_organization_group"
		return
	}
	resp.TypeName = req.ProviderTypeName + "_team_directory_group"
}

func (r *groupMembersResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	if r.kind == groupKindOrganization {
		resp.TypeName = req.ProviderTypeName + "_organization_group_members"
		return
	}
	resp.TypeName = req.ProviderTypeName + "_team_directory_group_members"
}

type groupNameValidator struct{}

func (groupNameValidator) Description(context.Context) string {
	return "must not be empty or start or end with whitespace"
}

func (v groupNameValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (groupNameValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	name := req.ConfigValue.ValueString()
	if name == "" || strings.TrimSpace(name) != name {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid group name", "name must not be empty or start or end with whitespace. The API trims the name, so the stored name would differ from the configuration.")
	}
}

// keepKnownState is UseStateForUnknown that leaves the value unknown while state has none, so a
// state written before the attribute existed is filled by the next apply instead of planned as null.
type keepKnownState struct{}

func (keepKnownState) Description(context.Context) string {
	return "Once set, the value of this attribute in state will not change."
}

func (m keepKnownState) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (keepKnownState) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.State.Raw.IsNull() || !req.PlanValue.IsUnknown() || req.ConfigValue.IsUnknown() || req.StateValue.IsNull() {
		return
	}
	resp.PlanValue = req.StateValue
}

func (r *groupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	desc := "Manages a manual Team directory group. The API can create manual groups only; SCIM-synced groups are created by the identity provider. A SCIM-synced group can be imported to manage its spending limit, but the API returns 400 on its member changes and on delete while a SCIM mapping exists, and nothing in the API response marks a group as SCIM-synced. monthly_spending_limit_dollars is one limit on the group, applied to each member. Null means no limit. 0 is a zero-dollar limit. Group names are unique among the team's active groups, so create fails when the name is taken; import the existing group instead. Delete requires the group to be empty, so destroy cursor_team_directory_group_members first. Refresh drops the resource from state when the group no longer exists. The id uses the team_group_ prefix. Import with that id or with the grp_ public id."
	if r.kind == groupKindOrganization {
		desc = "Manages a manual Organization group. The API can create manual groups only; SCIM-synced groups are synced from the identity provider. A SCIM-synced group can be imported to manage its spending limit, but the API returns 400 on its member changes and on delete while a SCIM mapping exists, and nothing in the API response marks a group as SCIM-synced. monthly_spending_limit_dollars is one limit on the group, applied to each member. Null means no limit. 0 is a zero-dollar limit. Group names are unique among the organization's active groups, so create fails when the name is taken; import the existing group instead. Delete requires the group to be empty, so destroy cursor_organization_group_members first. Refresh drops the resource from state when the group no longer exists. id uses the g_ prefix. public_id uses the grp_ prefix. Origin grants reference public_id, not id. Import with the g_ id. Requires organization_id and organization_api_key."
	}
	attrs := map[string]schema.Attribute{
		"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "Group id."},
		"name": schema.StringAttribute{
			Required:    true,
			Validators:  []validator.String{groupNameValidator{}},
			Description: "Group name. Must not be empty or start or end with whitespace. Unique among the active groups of the " + r.kind.owner() + ".",
		},
		"public_id":    schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{keepKnownState{}}, Description: "Public id with the grp_ prefix. Origin grants use this value."},
		"member_count": schema.Int64Attribute{Computed: true, Description: "Number of members at the last refresh."},
		"monthly_spending_limit_dollars": schema.Int64Attribute{
			Optional:    true,
			Validators:  []validator.Int64{int64validator.Between(0, maxGroupSpendingLimitDollars)},
			Description: "Monthly spending limit in whole dollars for each member, from 0 to 2147483647. Null means no limit; removing the attribute sends clearMonthlySpendingLimitDollars. 0 is a zero-dollar limit.",
		},
	}
	if r.kind == groupKindOrganization {
		attrs["organization_id"] = schema.StringAttribute{Computed: true, Description: "Provider organization_id this group was managed with."}
	}
	resp.Schema = schema.Schema{Description: desc, Attributes: attrs}
}

func (r *groupMembersResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	desc := "Owns the complete member set of one manual Team directory group. Declare it at most once per group. Create fails when the group already has members that are not in user_ids; import the resource to take over an existing member set. Add and remove use bulk-add and bulk-remove, up to 100 user ids per call. The API accepts and ignores user ids outside the team, so an apply fails when an id it sent is not a member afterwards. Refresh reads every page of the member list, so members added elsewhere are planned for removal, and drops the resource from state when the group no longer exists. Destroy removes every member of the group, including members added outside Terraform. Delete of the group requires the group to be empty, so destroy this resource first. Member changes return 400 on a SCIM-synced group; manage that membership in the identity provider and do not declare this resource for it. Import with the team_group_ id of the group."
	if r.kind == groupKindOrganization {
		desc = "Owns the complete member set of one manual Organization group. Declare it at most once per group. Create fails when the group already has members that are not in user_ids; import the resource to take over an existing member set. Add and remove use bulk-add and bulk-remove, up to 100 user ids per call. The API accepts and ignores user ids outside the organization, so an apply fails when an id it sent is not a member afterwards. Refresh reads every page of the member list, so members added elsewhere are planned for removal, and drops the resource from state when the group no longer exists. Destroy removes every member of the group, including members added outside Terraform. Delete of the group requires the group to be empty, so destroy this resource first. Member changes return 400 on a SCIM-synced group; manage that membership in the identity provider and do not declare this resource for it. Routes use the group id (g_), not public_id. Import with the g_ id of the group."
	}
	idPattern, idMessage := r.kind.idPattern()
	resp.Schema = schema.Schema{
		Description: desc,
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "Group id."},
			"group_id": schema.StringAttribute{
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.RegexMatches(idPattern, idMessage)},
				Description:   "Group id, not the grp_ public id. Changing it replaces the resource.",
			},
			"user_ids": schema.SetAttribute{
				ElementType: types.StringType,
				Required:    true,
				Validators: []validator.Set{
					setvalidator.NoNullValues(),
					setvalidator.ValueStringsAre(stringvalidator.RegexMatches(groupUserIDPattern, "must be a user id, not an email, and must not be empty or contain whitespace")),
				},
				Description: "Public user ids, such as user_abc123. This is the complete member set; an empty set keeps the group empty.",
			},
		},
	}
}

func (r *groupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client, resp.Diagnostics = configureClient(req, resp.Diagnostics)
}
func (r *groupMembersResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client, resp.Diagnostics = configureClient(req, resp.Diagnostics)
}

func (r *groupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	plan, diags := r.getGroup(ctx, req.Plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	name := plan.Name.ValueString()
	created, err := r.client.createCursorGroup(ctx, apiName, key, collection, name)
	if err != nil {
		detail := errorSentence(err)
		if isAdminBadRequest(err) {
			detail += fmt.Sprintf(" The API returns 400 when the name is already used by another active group of the %s; import that group instead of creating it.", r.kind.owner())
		}
		resp.Diagnostics.AddError("Failed to create group", detail)
		return
	}
	if created.ID == "" {
		resp.Diagnostics.AddError("Failed to create group", fmt.Sprintf("The create response did not include the group id. If a group named %q now exists, import it.", name))
		return
	}
	group := created
	problem := ""
	if want := groupLimit(plan.MonthlySpendingLimitDollars); want != nil {
		updated, err := r.patch(ctx, created.ID, map[string]any{"monthlySpendingLimitDollars": *want})
		if err != nil {
			problem = "setting its spending limit failed: " + err.Error()
		} else {
			group = updated
		}
	}
	if problem == "" {
		problem = groupMismatch(plan, group)
	}
	if problem == "" {
		resp.Diagnostics.Append(r.setGroup(ctx, &resp.State, group)...)
		return
	}
	// Without a rollback the half-configured group is tainted, and replacing it with
	// create_before_destroy can never succeed because the name is still taken.
	undoErr := r.client.deleteCursorGroup(ctx, apiName, key, collection, created.ID)
	if undoErr == nil || isAdminNotFound(undoErr) {
		resp.Diagnostics.AddError("Failed to create group", fmt.Sprintf("Group %s was created, but %s. The group was deleted again, so nothing is recorded in state.", created.ID, problem))
		return
	}
	resp.Diagnostics.Append(r.setGroup(ctx, &resp.State, group)...)
	resp.Diagnostics.AddError("Failed to create group", fmt.Sprintf("Group %s was created, but %s. Deleting it again also failed (%s), so the group is recorded in state as tainted and the next apply replaces it. Run terraform untaint on the resource to keep the group and update it in place instead.", created.ID, problem, undoErr))
}

func (r *groupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	state, diags := r.getGroup(ctx, req.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	group, err := r.client.getCursorGroup(ctx, apiName, key, collection, state.ID.ValueString())
	if isAdminNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read group", err.Error())
		return
	}
	if group.ID == "" {
		resp.Diagnostics.AddError("Failed to read group", fmt.Sprintf("The response for group %s did not include a group.", state.ID.ValueString()))
		return
	}
	resp.Diagnostics.Append(r.setGroup(ctx, &resp.State, group)...)
}

func (r *groupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	plan, diags := r.getGroup(ctx, req.Plan)
	resp.Diagnostics.Append(diags...)
	prior, diags := r.getGroup(ctx, req.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	id := prior.ID.ValueString()
	var group *cursorGroup
	if payload := groupPatch(plan, prior); len(payload) > 0 {
		group, err = r.patch(ctx, id, payload)
		if err != nil {
			resp.Diagnostics.AddError("Failed to update group", err.Error())
			return
		}
	} else {
		group, err = r.client.getCursorGroup(ctx, apiName, key, collection, id)
		if err == nil && group.ID == "" {
			err = fmt.Errorf("the response for group %s did not include a group", id)
		}
		if err != nil {
			resp.Diagnostics.AddError("Failed to read group", err.Error())
			return
		}
	}
	if group.PublicID == "" {
		group.PublicID = prior.PublicID.ValueString()
	}
	resp.Diagnostics.Append(r.setGroup(ctx, &resp.State, group)...)
	// Only the planned difference is sent. When prior state was stale the result can still differ
	// from the plan; that is reported instead of patched, so the next plan shows the change.
	if problem := groupMismatch(plan, group); problem != "" {
		resp.Diagnostics.AddError("Failed to update group", fmt.Sprintf("The update of group %s was accepted, but %s. State records what the API returned, so the next plan shows the remaining difference.", id, problem))
	}
}

func (r *groupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	state, diags := r.getGroup(ctx, req.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	err = r.client.deleteCursorGroup(ctx, apiName, key, collection, state.ID.ValueString())
	if err == nil || isAdminNotFound(err) {
		return
	}
	detail := errorSentence(err)
	if isAdminBadRequest(err) {
		detail += fmt.Sprintf(" The API refuses to delete a group that still has members or has an active SCIM mapping. Remove the members first, for example by destroying the matching %s resource. For a SCIM-synced group, remove the mapping in the dashboard, or drop the resource from state without deleting the group (terraform state rm, or a removed block with destroy = false).", r.membersTypeName())
	}
	resp.Diagnostics.AddError("Failed to delete group", detail)
}

func (r *groupResource) membersTypeName() string {
	if r.kind == groupKindOrganization {
		return "cursor_organization_group_members"
	}
	return "cursor_team_directory_group_members"
}

func (r *groupResource) endpoint() (apiName, key, collection string, err error) {
	if r.kind == groupKindOrganization {
		if err = r.client.requireOrgKey(); err != nil {
			return "", "", "", err
		}
		return "Organization Admin API", r.client.orgAPIKey, "/organizations/groups", nil
	}
	if err = r.client.requireTeamKey(); err != nil {
		return "", "", "", err
	}
	return "Team Admin API", r.client.teamAPIKey, "/teams/directory-groups", nil
}

// patch returns the group after the update. A patch response without a group is followed by a read.
func (r *groupResource) patch(ctx context.Context, id string, payload map[string]any) (*cursorGroup, error) {
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		return nil, err
	}
	group, err := r.client.patchCursorGroup(ctx, apiName, key, collection, id, payload)
	if err != nil {
		return nil, err
	}
	if group.ID != "" {
		return group, nil
	}
	group, err = r.client.getCursorGroup(ctx, apiName, key, collection, id)
	if err != nil {
		return nil, fmt.Errorf("reading the group after the update: %w", err)
	}
	if group.ID == "" {
		return nil, fmt.Errorf("the response for group %s did not include a group", id)
	}
	return group, nil
}

func (r *groupResource) getGroup(ctx context.Context, src attrGetter) (groupModel, diag.Diagnostics) {
	if r.kind == groupKindOrganization {
		var org orgGroupModel
		diags := src.Get(ctx, &org)
		return groupModel{
			ID:                          org.ID,
			Name:                        org.Name,
			PublicID:                    org.PublicID,
			MemberCount:                 org.MemberCount,
			MonthlySpendingLimitDollars: org.MonthlySpendingLimitDollars,
		}, diags
	}
	var model groupModel
	diags := src.Get(ctx, &model)
	return model, diags
}

func (r *groupResource) setGroup(ctx context.Context, state *tfsdk.State, group *cursorGroup) diag.Diagnostics {
	model := groupModel{
		ID:                          types.StringValue(group.ID),
		Name:                        types.StringValue(group.Name),
		PublicID:                    types.StringNull(),
		MemberCount:                 types.Int64Value(group.MemberCount),
		MonthlySpendingLimitDollars: types.Int64Null(),
	}
	if group.PublicID != "" {
		model.PublicID = types.StringValue(group.PublicID)
	}
	if group.MonthlySpendingLimitDollars != nil {
		model.MonthlySpendingLimitDollars = types.Int64Value(*group.MonthlySpendingLimitDollars)
	}
	if r.kind != groupKindOrganization {
		return state.Set(ctx, &model)
	}
	orgID := types.StringNull()
	if r.client != nil && r.client.organizationID != "" {
		orgID = types.StringValue(r.client.organizationID)
	}
	return state.Set(ctx, &orgGroupModel{
		ID:                          model.ID,
		Name:                        model.Name,
		PublicID:                    model.PublicID,
		OrganizationID:              orgID,
		MemberCount:                 model.MemberCount,
		MonthlySpendingLimitDollars: model.MonthlySpendingLimitDollars,
	})
}

func groupLimit(value types.Int64) *int64 {
	if value.IsNull() || value.IsUnknown() {
		return nil
	}
	limit := value.ValueInt64()
	return &limit
}

func groupLimitText(limit *int64) string {
	if limit == nil {
		return "none"
	}
	return fmt.Sprintf("%d dollars", *limit)
}

// groupPatch is the partial update that moves prior state to the plan. The API rejects a request
// that both sets and clears the limit, and one with no fields.
func groupPatch(plan, prior groupModel) map[string]any {
	payload := map[string]any{}
	if !plan.Name.Equal(prior.Name) {
		payload["name"] = plan.Name.ValueString()
	}
	want, have := groupLimit(plan.MonthlySpendingLimitDollars), groupLimit(prior.MonthlySpendingLimitDollars)
	switch {
	case want == nil && have != nil:
		payload["clearMonthlySpendingLimitDollars"] = true
	case want != nil && (have == nil || *have != *want):
		payload["monthlySpendingLimitDollars"] = *want
	}
	return payload
}

// groupMismatch describes how the group the API returned differs from the plan, or is empty.
func groupMismatch(plan groupModel, group *cursorGroup) string {
	var parts []string
	if name := plan.Name.ValueString(); group.Name != name {
		parts = append(parts, fmt.Sprintf("the API stored the name %q instead of %q", group.Name, name))
	}
	want, have := groupLimit(plan.MonthlySpendingLimitDollars), group.MonthlySpendingLimitDollars
	if (want == nil) != (have == nil) || (want != nil && *want != *have) {
		parts = append(parts, fmt.Sprintf("the API reports a spending limit of %s instead of %s", groupLimitText(have), groupLimitText(want)))
	}
	return strings.Join(parts, " and ")
}

func errorSentence(err error) string {
	return strings.TrimRight(err.Error(), ". ") + "."
}

func isAdminBadRequest(err error) bool {
	var apiErr *adminStatusError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest
}

func (r *groupMembersResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan groupMembersModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, _, _, err := r.endpoint(); err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	groupID := plan.GroupID.ValueString()
	desired, diags := userIDValues(ctx, plan.UserIDs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	existing, err := r.list(ctx, groupID)
	if isAdminNotFound(err) {
		resp.Diagnostics.AddError("Group not found", fmt.Sprintf("%s %s does not exist, so no members were added: %s", r.kind.noun(), groupID, err))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read group members", err.Error())
		return
	}
	if unmanaged := difference(existing, desired); len(unmanaged) > 0 {
		resp.Diagnostics.AddError("Group already has members", fmt.Sprintf("%s %s already has %d member(s) that are not in user_ids: %s. This resource owns the complete member set and nothing was changed. Import it with `terraform import <address> %s` so the next plan shows those members as removals, or add them to user_ids.", r.kind.noun(), groupID, len(unmanaged), describeIDs(unmanaged), groupID))
		return
	}
	result, missing, err := r.add(ctx, groupID, difference(desired, existing))
	if err == nil && len(missing) == 0 {
		resp.Diagnostics.Append(setMembersState(ctx, &resp.State, groupID, desired)...)
		return
	}
	detail := r.addFailure(groupID, missing, err)
	if len(result.sent) == 0 {
		resp.Diagnostics.AddError("Failed to add group members", detail+" No member was added, so nothing is recorded in state.")
		return
	}
	_, undoErr := r.bulk(ctx, groupID, "bulk-remove", result.sent)
	if undoErr == nil || isAdminNotFound(undoErr) {
		resp.Diagnostics.AddError("Failed to add group members", detail+" The members this apply added were removed again, so nothing is recorded in state.")
		return
	}
	resp.Diagnostics.Append(r.recordActual(ctx, &resp.State, groupID, append(slices.Clone(existing), result.sent...))...)
	resp.Diagnostics.AddError("Failed to add group members", fmt.Sprintf("%s Removing the members this apply added also failed (%s), so the current member set is recorded in state as tainted. The next apply removes every member and adds user_ids again.", detail, undoErr))
}

func (r *groupMembersResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state groupMembersModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, _, _, err := r.endpoint(); err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	ids, err := r.list(ctx, state.GroupID.ValueString())
	if isAdminNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read group members", err.Error())
		return
	}
	resp.Diagnostics.Append(setMembersState(ctx, &resp.State, state.GroupID.ValueString(), ids)...)
}

func (r *groupMembersResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior groupMembersModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, _, _, err := r.endpoint(); err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	groupID := plan.GroupID.ValueString()
	desired, diags := userIDValues(ctx, plan.UserIDs)
	resp.Diagnostics.Append(diags...)
	current, diags := userIDValues(ctx, prior.UserIDs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	removed, err := r.bulk(ctx, groupID, "bulk-remove", difference(current, desired))
	if err != nil {
		resp.Diagnostics.Append(r.recordActual(ctx, &resp.State, groupID, difference(current, removed.sent))...)
		resp.Diagnostics.AddError("Failed to remove group members", r.writeFailure(err)+" State records the current member set.")
		return
	}
	kept := difference(current, removed.sent)
	added, missing, err := r.add(ctx, groupID, difference(desired, current))
	if err != nil || len(missing) > 0 {
		resp.Diagnostics.Append(r.recordActual(ctx, &resp.State, groupID, append(kept, difference(added.sent, missing)...))...)
		resp.Diagnostics.AddError("Failed to add group members", r.addFailure(groupID, missing, err)+" State records the current member set.")
		return
	}
	resp.Diagnostics.Append(setMembersState(ctx, &resp.State, groupID, desired)...)
}

func (r *groupMembersResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state groupMembersModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, _, _, err := r.endpoint(); err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	groupID := state.GroupID.ValueString()
	members, err := r.list(ctx, groupID)
	if isAdminNotFound(err) {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read group members", err.Error())
		return
	}
	// The group can only be deleted once it is empty, so destroy removes every listed member
	// and not only the ids in state.
	ids, _ := userIDValues(ctx, state.UserIDs)
	ids = uniqueStrings(append(ids, members...))
	if _, err := r.bulk(ctx, groupID, "bulk-remove", ids); err != nil && !isAdminNotFound(err) {
		detail := r.writeFailure(err)
		if isAdminBadRequest(err) {
			detail += " To stop managing the membership without removing anyone, drop the resource from state (terraform state rm, or a removed block with destroy = false)."
		}
		resp.Diagnostics.AddError("Failed to remove group members", detail)
	}
}

func (r *groupMembersResource) endpoint() (string, string, string, error) {
	return (&groupResource{client: r.client, kind: r.kind}).endpoint()
}

func (r *groupMembersResource) list(ctx context.Context, groupID string) ([]string, error) {
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		return nil, err
	}
	members, err := r.client.listGroupMembers(ctx, apiName, key, collection, groupID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.UserID)
	}
	return ids, nil
}

func (r *groupMembersResource) bulk(ctx context.Context, groupID, action string, ids []string) (bulkMembersResult, error) {
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		return bulkMembersResult{}, err
	}
	return r.client.bulkGroupMembers(ctx, apiName, key, collection, groupID, action, ids)
}

// add posts ids and returns the ones the group still lacks. The API accepts and ignores ids
// outside the team or organization, so unless addedCount accounts for every id the member list
// is read back. A short count alone is not a failure: an id that was already a member is not counted.
func (r *groupMembersResource) add(ctx context.Context, groupID string, ids []string) (bulkMembersResult, []string, error) {
	result, err := r.bulk(ctx, groupID, "bulk-add", ids)
	if err != nil || len(ids) == 0 || (result.counted && result.changed == int64(len(ids))) {
		return result, nil, err
	}
	members, err := r.list(ctx, groupID)
	if err != nil {
		return result, nil, fmt.Errorf("the add was accepted, but reading the members back failed: %w", err)
	}
	return result, difference(ids, members), nil
}

func (r *groupMembersResource) addFailure(groupID string, missing []string, err error) string {
	if err != nil {
		return r.writeFailure(err)
	}
	return fmt.Sprintf("The API accepted the request, but %d user id(s) are not members of %s %s afterwards: %s. The API ignores user ids that do not belong to the %s.", len(missing), r.kind.noun(), groupID, describeIDs(missing), r.kind.owner())
}

func (r *groupMembersResource) writeFailure(err error) string {
	detail := errorSentence(err)
	if isAdminBadRequest(err) {
		detail += " The API returns 400 for member changes on a SCIM-synced group; manage that group's membership in the identity provider."
	}
	return detail
}

// recordActual stores the member list as the API reports it after a failed write, or fallback
// when that read fails too.
func (r *groupMembersResource) recordActual(ctx context.Context, state *tfsdk.State, groupID string, fallback []string) diag.Diagnostics {
	ids, err := r.list(ctx, groupID)
	if err != nil {
		ids = fallback
	}
	return setMembersState(ctx, state, groupID, ids)
}

func setMembersState(ctx context.Context, state *tfsdk.State, groupID string, ids []string) diag.Diagnostics {
	model, diags := membersState(ctx, groupID, ids)
	if diags.HasError() {
		return diags
	}
	return append(diags, state.Set(ctx, model)...)
}

func membersState(ctx context.Context, groupID string, ids []string) (*groupMembersModel, diag.Diagnostics) {
	unique := uniqueStrings(ids)
	if unique == nil {
		unique = []string{}
	}
	set, diags := types.SetValueFrom(ctx, types.StringType, unique)
	if diags.HasError() {
		return nil, diags
	}
	return &groupMembersModel{ID: types.StringValue(groupID), GroupID: types.StringValue(groupID), UserIDs: set}, diags
}

// userIDValues returns the ids of a fully known set in sorted order.
func userIDValues(_ context.Context, set types.Set) ([]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if set.IsNull() {
		return nil, diags
	}
	if set.IsUnknown() {
		diags.AddAttributeError(path.Root("user_ids"), "Unknown user ids", "user_ids is not known, so the member set cannot be compared.")
		return nil, diags
	}
	ids := make([]string, 0, len(set.Elements()))
	for _, element := range set.Elements() {
		value, ok := element.(types.String)
		if !ok || value.IsNull() || value.IsUnknown() {
			diags.AddAttributeError(path.Root("user_ids"), "Invalid user id", "user_ids must contain only known, non-null strings.")
			return nil, diags
		}
		ids = append(ids, value.ValueString())
	}
	slices.Sort(ids)
	return ids, diags
}

func describeIDs(ids []string) string {
	const limit = 10
	if len(ids) <= limit {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:limit], ", "), len(ids)-limit)
}

func stringValues(ctx context.Context, list types.List) []string {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	var values []string
	if diags := list.ElementsAs(ctx, &values, false); diags.HasError() {
		return nil
	}
	return values
}

func difference(from, subtract []string) []string {
	skip := map[string]struct{}{}
	for _, value := range subtract {
		skip[value] = struct{}{}
	}
	var out []string
	for _, value := range from {
		if _, ok := skip[value]; !ok {
			out = append(out, value)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

var (
	_ resource.Resource = (*groupResource)(nil)
	_ resource.Resource = (*groupMembersResource)(nil)
)
