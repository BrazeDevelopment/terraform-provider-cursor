package provider

import (
	"context"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type groupKind int

const (
	groupKindTeam groupKind = iota
	groupKindOrganization
)

type groupResource struct {
	client *apiClient
	kind   groupKind
}

type groupModel struct {
	ID                          types.String `tfsdk:"id"`
	Name                        types.String `tfsdk:"name"`
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

func (r *groupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	desc := "Manages a manual Team directory group. The API can create manual groups only. SCIM-synced groups are created by the identity provider; member changes and delete return 400 while a SCIM mapping exists. monthly_spending_limit_dollars is one limit on the group, applied to each member. Null means no limit. 0 is a zero-dollar limit. Delete requires the group to be empty, so destroy cursor_team_directory_group_members first. The id uses the team_group_ prefix."
	if r.kind == groupKindOrganization {
		desc = "Manages an Organization group. id uses the g_ prefix. public_id uses the grp_ prefix. Origin grants reference public_id, not id. Requires organization_id and organization_api_key."
	}
	attrs := map[string]schema.Attribute{
		"id":           schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "Group id."},
		"name":         schema.StringAttribute{Required: true, Description: "Group name."},
		"member_count": schema.Int64Attribute{Computed: true, Description: "Number of members."},
		"monthly_spending_limit_dollars": schema.Int64Attribute{
			Optional:    true,
			Description: "Monthly spending limit in whole dollars for each member. Null means no limit and sends clearMonthlySpendingLimitDollars. 0 is a zero-dollar limit.",
		},
	}
	if r.kind == groupKindOrganization {
		attrs["public_id"] = schema.StringAttribute{Computed: true, Description: "Public id with the grp_ prefix. Origin grants use this value."}
		attrs["organization_id"] = schema.StringAttribute{Computed: true, Description: "Provider organization_id this group was managed with."}
	}
	resp.Schema = schema.Schema{Description: desc, Attributes: attrs}
}

func (r *groupMembersResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	desc := "Owns the member set of one manual Team directory group. Add and remove use bulk-add and bulk-remove, up to 100 user ids per call. Delete of the group requires the group to be empty, so destroy this resource first. Member changes return 400 on a SCIM-synced group."
	if r.kind == groupKindOrganization {
		desc = "Owns the member set of one manual Organization group. Routes use the group id (g_), not public_id."
	}
	resp.Schema = schema.Schema{
		Description: desc,
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}, Description: "Group id."},
			"group_id": schema.StringAttribute{
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Description:   "Group id.",
			},
			"user_ids": schema.SetAttribute{
				ElementType: types.StringType,
				Required:    true,
				Description: "Public user ids.",
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
	created, err := r.client.createCursorGroup(ctx, apiName, key, collection, plan.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to create group", err.Error())
		return
	}
	if !plan.MonthlySpendingLimitDollars.IsNull() && !plan.MonthlySpendingLimitDollars.IsUnknown() {
		limit := plan.MonthlySpendingLimitDollars.ValueInt64()
		updated, err := r.client.patchCursorGroup(ctx, apiName, key, collection, created.ID, map[string]any{"monthlySpendingLimitDollars": limit})
		if err != nil {
			resp.Diagnostics.AddError("Failed to set group spending limit", err.Error())
			return
		}
		created = updated
	}
	resp.Diagnostics.Append(r.setGroup(ctx, &resp.State, plan, created)...)
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
	resp.Diagnostics.Append(r.setGroup(ctx, &resp.State, state, group)...)
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
	payload := map[string]any{}
	if !plan.Name.Equal(prior.Name) {
		payload["name"] = plan.Name.ValueString()
	}
	switch {
	case plan.MonthlySpendingLimitDollars.IsNull() && !prior.MonthlySpendingLimitDollars.IsNull():
		payload["clearMonthlySpendingLimitDollars"] = true
	case !plan.MonthlySpendingLimitDollars.IsNull() && !plan.MonthlySpendingLimitDollars.Equal(prior.MonthlySpendingLimitDollars):
		payload["monthlySpendingLimitDollars"] = plan.MonthlySpendingLimitDollars.ValueInt64()
	}
	group := &cursorGroup{ID: prior.ID.ValueString(), Name: plan.Name.ValueString()}
	if len(payload) > 0 {
		group, err = r.client.patchCursorGroup(ctx, apiName, key, collection, prior.ID.ValueString(), payload)
		if err != nil {
			resp.Diagnostics.AddError("Failed to update group", err.Error())
			return
		}
	} else {
		group, err = r.client.getCursorGroup(ctx, apiName, key, collection, prior.ID.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Failed to read group", err.Error())
			return
		}
	}
	resp.Diagnostics.Append(r.setGroup(ctx, &resp.State, plan, group)...)
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
	if err := r.client.deleteCursorGroup(ctx, apiName, key, collection, state.ID.ValueString()); err != nil && !isAdminNotFound(err) {
		resp.Diagnostics.AddError("Failed to delete group", err.Error())
	}
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

func (r *groupResource) getGroup(ctx context.Context, src attrGetter) (groupModel, diag.Diagnostics) {
	if r.kind == groupKindOrganization {
		var org orgGroupModel
		diags := src.Get(ctx, &org)
		return groupModel{
			ID:                          org.ID,
			Name:                        org.Name,
			MemberCount:                 org.MemberCount,
			MonthlySpendingLimitDollars: org.MonthlySpendingLimitDollars,
		}, diags
	}
	var model groupModel
	diags := src.Get(ctx, &model)
	return model, diags
}

func (r *groupResource) setGroup(ctx context.Context, state *tfsdk.State, prior groupModel, group *cursorGroup) diag.Diagnostics {
	prior.ID = types.StringValue(group.ID)
	prior.Name = types.StringValue(group.Name)
	prior.MemberCount = types.Int64Value(group.MemberCount)
	if group.MonthlySpendingLimitDollars == nil {
		prior.MonthlySpendingLimitDollars = types.Int64Null()
	} else {
		prior.MonthlySpendingLimitDollars = types.Int64Value(*group.MonthlySpendingLimitDollars)
	}
	if r.kind != groupKindOrganization {
		return state.Set(ctx, &prior)
	}
	publicID := types.StringNull()
	if group.PublicID != "" {
		publicID = types.StringValue(group.PublicID)
	}
	orgID := types.StringNull()
	if r.client != nil && r.client.organizationID != "" {
		orgID = types.StringValue(r.client.organizationID)
	}
	return state.Set(ctx, &orgGroupModel{
		ID:                          prior.ID,
		Name:                        prior.Name,
		PublicID:                    publicID,
		OrganizationID:              orgID,
		MemberCount:                 prior.MemberCount,
		MonthlySpendingLimitDollars: prior.MonthlySpendingLimitDollars,
	})
}

func (r *groupMembersResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan groupMembersModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	ids := setValues(ctx, plan.UserIDs)
	if err := r.client.bulkGroupMembers(ctx, apiName, key, collection, plan.GroupID.ValueString(), "bulk-add", ids); err != nil {
		resp.Diagnostics.AddError("Failed to add group members", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, membersState(plan.GroupID.ValueString(), ids))...)
}

func (r *groupMembersResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state groupMembersModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	members, err := r.client.listGroupMembers(ctx, apiName, key, collection, state.GroupID.ValueString())
	if isAdminNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Failed to read group members", err.Error())
		return
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.UserID)
	}
	slices.Sort(ids)
	resp.Diagnostics.Append(resp.State.Set(ctx, membersState(state.GroupID.ValueString(), ids))...)
}

func (r *groupMembersResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior groupMembersModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	desired := setValues(ctx, plan.UserIDs)
	current := setValues(ctx, prior.UserIDs)
	if err := r.client.bulkGroupMembers(ctx, apiName, key, collection, plan.GroupID.ValueString(), "bulk-remove", difference(current, desired)); err != nil {
		resp.Diagnostics.AddError("Failed to remove group members", err.Error())
		return
	}
	if err := r.client.bulkGroupMembers(ctx, apiName, key, collection, plan.GroupID.ValueString(), "bulk-add", difference(desired, current)); err != nil {
		resp.Diagnostics.AddError("Failed to add group members", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, membersState(plan.GroupID.ValueString(), desired))...)
}

func (r *groupMembersResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state groupMembersModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	apiName, key, collection, err := r.endpoint()
	if err != nil {
		resp.Diagnostics.AddError("Provider not configured", err.Error())
		return
	}
	members, err := r.client.listGroupMembers(ctx, apiName, key, collection, state.GroupID.ValueString())
	if err != nil && !isAdminNotFound(err) {
		resp.Diagnostics.AddError("Failed to read group members", err.Error())
		return
	}
	ids := setValues(ctx, state.UserIDs)
	for _, member := range members {
		ids = append(ids, member.UserID)
	}
	if err := r.client.bulkGroupMembers(ctx, apiName, key, collection, state.GroupID.ValueString(), "bulk-remove", uniqueStrings(ids)); err != nil && !isAdminNotFound(err) {
		resp.Diagnostics.AddError("Failed to remove group members", err.Error())
	}
}

func (r *groupMembersResource) endpoint() (string, string, string, error) {
	return (&groupResource{client: r.client, kind: r.kind}).endpoint()
}

func membersState(groupID string, ids []string) *groupMembersModel {
	slices.Sort(ids)
	set, _ := types.SetValueFrom(context.Background(), types.StringType, ids)
	return &groupMembersModel{ID: types.StringValue(groupID), GroupID: types.StringValue(groupID), UserIDs: set}
}

func setValues(ctx context.Context, set types.Set) []string {
	if set.IsNull() || set.IsUnknown() {
		return nil
	}
	var values []string
	if diags := set.ElementsAs(ctx, &values, false); diags.HasError() {
		return nil
	}
	return values
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
	seen := map[string]struct{}{}
	var out []string
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
