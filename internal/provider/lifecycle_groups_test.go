package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

type lgCall struct {
	Method string
	Path   string
	Query  string
	Body   string
}

// lgFake is an in-memory Team directory group or Organization group API that follows the documented
// behaviour: names are trimmed, bulk-add ignores unknown users and existing members, delete needs an
// empty group, and SCIM-synced groups reject member changes and delete with 400.
type lgFake struct {
	mu         sync.Mutex
	kind       groupKind
	collection string
	seq        int
	groups     map[string]*cursorGroup
	members    map[string][]string
	scim       map[string]bool
	outsiders  map[string]bool
	calls      []lgCall

	// pagination selects the list metadata: "" sends the documented object, "totalPages" omits
	// hasNextPage, "none" omits the object, and "unpaged" also ignores the page parameter.
	pagination  string
	maxPageSize int
	omitCounts  bool
	ignoreLimit bool
	storeName   func(string) string
	fail        func(call lgCall) (int, string)
}

func newLgFake(t *testing.T, kind groupKind) (*lgFake, *apiClient) {
	t.Helper()
	fake := &lgFake{kind: kind, collection: "/teams/directory-groups", groups: map[string]*cursorGroup{}, members: map[string][]string{}, scim: map[string]bool{}, outsiders: map[string]bool{}}
	if kind == groupKindOrganization {
		fake.collection = "/organizations/groups"
	}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	client := &apiClient{httpClient: server.Client(), adminBase: server.URL, userAgent: "test"}
	if kind == groupKindOrganization {
		client.orgAPIKey, client.organizationID = "org-key", "org_1"
	} else {
		client.teamAPIKey = "team-key"
	}
	return fake, client
}

func (f *lgFake) addGroup(name string, limit *int64, members ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.newGroup(name, limit, members...).ID
}

func (f *lgFake) newGroup(name string, limit *int64, members ...string) *cursorGroup {
	f.seq++
	prefix := "team_group_"
	if f.kind == groupKindOrganization {
		prefix = "g_"
	}
	group := &cursorGroup{ID: prefix + strconv.Itoa(f.seq), PublicID: "grp_" + strconv.Itoa(f.seq), Name: name, MonthlySpendingLimitDollars: limit}
	f.groups[group.ID] = group
	f.members[group.ID] = slices.Clone(members)
	return group
}

func (f *lgFake) memberIDs(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.members[id])
	slices.Sort(out)
	return out
}

func (f *lgFake) writes() []lgCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []lgCall
	for _, call := range f.calls {
		if call.Method != http.MethodGet {
			out = append(out, call)
		}
	}
	return out
}

func (f *lgFake) count(method, suffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, call := range f.calls {
		if call.Method == method && strings.HasSuffix(call.Path, suffix) {
			n++
		}
	}
	return n
}

func (f *lgFake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func (f *lgFake) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	call := lgCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(raw)}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	status, body := f.respond(call, r)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *lgFake) respond(call lgCall, r *http.Request) (int, string) {
	if f.fail != nil {
		if status, body := f.fail(call); status != 0 {
			return status, body
		}
	}
	rest, ok := strings.CutPrefix(call.Path, f.collection)
	if !ok {
		return http.StatusNotFound, `{"error":"unknown route"}`
	}
	if rest == "" {
		if call.Method != http.MethodPost {
			return http.StatusMethodNotAllowed, `{}`
		}
		var in struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal([]byte(call.Body), &in)
		name := strings.TrimSpace(in.Name)
		if f.storeName != nil {
			name = f.storeName(name)
		}
		for _, group := range f.groups {
			if group.Name == name {
				return http.StatusBadRequest, `{"error":"name already used by another active group"}`
			}
		}
		return http.StatusCreated, f.groupJSON(f.newGroup(name, nil))
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	group := f.groups[parts[0]]
	if group == nil && f.kind == groupKindTeam && call.Method == http.MethodGet {
		for _, candidate := range f.groups {
			if candidate.PublicID == parts[0] {
				group = candidate
			}
		}
	}
	if group == nil {
		return http.StatusNotFound, `{"error":"group not found"}`
	}
	switch {
	case len(parts) == 1 && call.Method == http.MethodGet:
		return http.StatusOK, f.groupJSON(group)
	case len(parts) == 1 && call.Method == http.MethodPatch:
		var in struct {
			Name  *string `json:"name"`
			Limit *int64  `json:"monthlySpendingLimitDollars"`
			Clear bool    `json:"clearMonthlySpendingLimitDollars"`
		}
		_ = json.Unmarshal([]byte(call.Body), &in)
		if (in.Name == nil && in.Limit == nil && !in.Clear) || (in.Limit != nil && in.Clear) {
			return http.StatusBadRequest, `{"error":"invalid update"}`
		}
		if in.Name != nil {
			group.Name = strings.TrimSpace(*in.Name)
		}
		if in.Limit != nil && !f.ignoreLimit {
			group.MonthlySpendingLimitDollars = in.Limit
		}
		if in.Clear {
			group.MonthlySpendingLimitDollars = nil
		}
		return http.StatusOK, f.groupJSON(group)
	case len(parts) == 1 && call.Method == http.MethodDelete:
		if len(f.members[group.ID]) > 0 || f.scim[group.ID] {
			return http.StatusBadRequest, `{"error":"group cannot be deleted"}`
		}
		delete(f.groups, group.ID)
		delete(f.members, group.ID)
		return http.StatusNoContent, ``
	case len(parts) == 2 && parts[1] == "members" && call.Method == http.MethodGet:
		return http.StatusOK, f.membersJSON(group.ID, r)
	case len(parts) == 3 && parts[1] == "members" && call.Method == http.MethodPost:
		if f.scim[group.ID] {
			return http.StatusBadRequest, `{"error":"membership cannot be changed"}`
		}
		var in struct {
			UserIDs []string `json:"userIds"`
		}
		_ = json.Unmarshal([]byte(call.Body), &in)
		if len(in.UserIDs) == 0 || len(in.UserIDs) > 100 {
			return http.StatusBadRequest, `{"error":"userIds must hold 1 to 100 ids"}`
		}
		changed := 0
		for _, id := range in.UserIDs {
			at := slices.Index(f.members[group.ID], id)
			switch {
			case parts[2] == "bulk-add" && at < 0 && !f.outsiders[id]:
				f.members[group.ID] = append(f.members[group.ID], id)
				changed++
			case parts[2] == "bulk-remove" && at >= 0:
				f.members[group.ID] = slices.Delete(f.members[group.ID], at, at+1)
				changed++
			}
		}
		if f.omitCounts {
			return http.StatusOK, `{}`
		}
		if parts[2] == "bulk-add" {
			return http.StatusOK, fmt.Sprintf(`{"addedCount":%d}`, changed)
		}
		return http.StatusOK, fmt.Sprintf(`{"removedCount":%d}`, changed)
	}
	return http.StatusNotFound, `{"error":"unknown route"}`
}

func (f *lgFake) groupJSON(group *cursorGroup) string {
	out := *group
	out.MemberCount = int64(len(f.members[group.ID]))
	raw, _ := json.Marshal(map[string]any{"group": out})
	return string(raw)
}

func (f *lgFake) membersJSON(id string, r *http.Request) string {
	all := f.members[id]
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if f.maxPageSize > 0 && (size == 0 || size > f.maxPageSize) {
		size = f.maxPageSize
	}
	if f.pagination == "unpaged" {
		page, size = 1, len(all)+1
	}
	start := min((page-1)*size, len(all))
	end := min(start+size, len(all))
	members := []map[string]string{}
	for _, userID := range all[start:end] {
		members = append(members, map[string]string{"userId": userID, "name": userID, "email": userID + "@example.com"})
	}
	totalPages := (len(all) + size - 1) / size
	out := map[string]any{"members": members}
	switch f.pagination {
	case "":
		out["pagination"] = map[string]any{"page": page, "pageSize": size, "totalCount": len(all), "totalPages": totalPages, "hasNextPage": page < totalPages, "hasPreviousPage": page > 1}
	case "totalPages":
		out["pagination"] = map[string]any{"page": page, "pageSize": size, "totalCount": len(all), "totalPages": totalPages}
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func lgEmptyState(ctx context.Context, sch schema.Schema) tfsdk.State {
	return tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
}

func lgGroupModel(kind groupKind, model groupModel) any {
	if kind == groupKindOrganization {
		organizationID := types.StringValue("org_1")
		if model.ID.IsUnknown() {
			organizationID = types.StringUnknown()
		}
		return &orgGroupModel{ID: model.ID, Name: model.Name, PublicID: model.PublicID, OrganizationID: organizationID, MemberCount: model.MemberCount, MonthlySpendingLimitDollars: model.MonthlySpendingLimitDollars}
	}
	return &model
}

func lgNewGroupPlan(name string, limit types.Int64) groupModel {
	return groupModel{ID: types.StringUnknown(), Name: types.StringValue(name), PublicID: types.StringUnknown(), MemberCount: types.Int64Unknown(), MonthlySpendingLimitDollars: limit}
}

func lgCreateGroup(t *testing.T, ctx context.Context, res *groupResource, plan groupModel) *resource.CreateResponse {
	t.Helper()
	sch := resourceSchema(t, res)
	resp := &resource.CreateResponse{State: lgEmptyState(ctx, sch)}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, lgGroupModel(res.kind, plan))}, resp)
	return resp
}

func lgReadGroup(t *testing.T, ctx context.Context, res *groupResource, state tfsdk.State) *resource.ReadResponse {
	t.Helper()
	resp := &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, resp)
	return resp
}

func lgUpdateGroup(t *testing.T, ctx context.Context, res *groupResource, prior, plan groupModel) *resource.UpdateResponse {
	t.Helper()
	sch := resourceSchema(t, res)
	state := mustState(t, ctx, sch, lgGroupModel(res.kind, prior))
	resp := &resource.UpdateResponse{State: state}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, lgGroupModel(res.kind, plan)), State: state}, resp)
	return resp
}

func lgGroup(t *testing.T, ctx context.Context, res *groupResource, state tfsdk.State) groupModel {
	t.Helper()
	model, diags := res.getGroup(ctx, state)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return model
}

func lgMembersModel(groupID string, ids ...string) groupMembersModel {
	set, _ := types.SetValueFrom(context.Background(), types.StringType, append([]string{}, ids...))
	return groupMembersModel{ID: types.StringValue(groupID), GroupID: types.StringValue(groupID), UserIDs: set}
}

func lgCreateMembers(t *testing.T, ctx context.Context, res *groupMembersResource, groupID string, ids ...string) *resource.CreateResponse {
	t.Helper()
	sch := resourceSchema(t, res)
	plan := lgMembersModel(groupID, ids...)
	plan.ID = types.StringUnknown()
	resp := &resource.CreateResponse{State: lgEmptyState(ctx, sch)}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	return resp
}

func lgUpdateMembers(t *testing.T, ctx context.Context, res *groupMembersResource, groupID string, prior, plan []string) *resource.UpdateResponse {
	t.Helper()
	sch := resourceSchema(t, res)
	priorModel, planModel := lgMembersModel(groupID, prior...), lgMembersModel(groupID, plan...)
	state := mustState(t, ctx, sch, &priorModel)
	resp := &resource.UpdateResponse{State: state}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &planModel), State: state}, resp)
	return resp
}

func lgStateIDs(t *testing.T, ctx context.Context, state tfsdk.State) []string {
	t.Helper()
	var model groupMembersModel
	if diags := state.Get(ctx, &model); diags.HasError() {
		t.Fatal(diags)
	}
	if model.UserIDs.IsNull() || model.UserIDs.IsUnknown() {
		t.Fatalf("user_ids = %v, want a known set", model.UserIDs)
	}
	ids := []string{}
	if diags := model.UserIDs.ElementsAs(ctx, &ids, false); diags.HasError() {
		t.Fatal(diags)
	}
	slices.Sort(ids)
	return ids
}

func lgUsers(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("user_%04d", i)
	}
	return ids
}

func lgWantError(t *testing.T, diags diag.Diagnostics, fragments ...string) {
	t.Helper()
	if !diags.HasError() {
		t.Fatalf("want an error mentioning %q, got none", fragments)
	}
	text := fmtDiag(diags)
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Fatalf("error %q does not mention %q", text, fragment)
		}
	}
}

func lgKinds() map[string]groupKind {
	return map[string]groupKind{"team": groupKindTeam, "organization": groupKindOrganization}
}

func TestGroupCreateSendsLimitAndKnowsEveryComputedAttribute(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		res := &groupResource{client: client, kind: kind}

		resp := lgCreateGroup(t, ctx, res, lgNewGroupPlan("Engineering", types.Int64Value(0)))
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: %v", name, resp.Diagnostics)
		}
		got := lgGroup(t, ctx, res, resp.State)
		idPattern, _ := kind.idPattern()
		if !idPattern.MatchString(got.ID.ValueString()) || got.PublicID.ValueString() != "grp_1" || got.Name.ValueString() != "Engineering" {
			t.Fatalf("%s: state = %#v", name, got)
		}
		if got.MonthlySpendingLimitDollars.IsNull() || got.MonthlySpendingLimitDollars.ValueInt64() != 0 || got.MemberCount.IsUnknown() || got.MemberCount.ValueInt64() != 0 {
			t.Fatalf("%s: limit = %v, member_count = %v", name, got.MonthlySpendingLimitDollars, got.MemberCount)
		}
		if kind == groupKindOrganization {
			var organizationID types.String
			resp.State.GetAttribute(ctx, path.Root("organization_id"), &organizationID)
			if organizationID.ValueString() != "org_1" {
				t.Fatalf("organization_id = %v", organizationID)
			}
		}
		writes := fake.writes()
		if len(writes) != 2 || writes[0].Body != `{"name":"Engineering"}` || writes[1].Method != http.MethodPatch || writes[1].Body != `{"monthlySpendingLimitDollars":0}` {
			t.Fatalf("%s: writes = %#v", name, writes)
		}

		fake.reset()
		resp = lgCreateGroup(t, ctx, res, lgNewGroupPlan("Design", types.Int64Null()))
		if resp.Diagnostics.HasError() || !lgGroup(t, ctx, res, resp.State).MonthlySpendingLimitDollars.IsNull() || len(fake.writes()) != 1 {
			t.Fatalf("%s: no-limit create diags %v writes %#v", name, resp.Diagnostics, fake.writes())
		}
	}
}

func TestGroupCreateRollsBackWhenTheLimitCannotBeSet(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		fake.fail = func(call lgCall) (int, string) {
			if call.Method == http.MethodPatch {
				return http.StatusInternalServerError, `{"error":"boom"}`
			}
			return 0, ""
		}
		res := &groupResource{client: client, kind: kind}
		resp := lgCreateGroup(t, ctx, res, lgNewGroupPlan("Engineering", types.Int64Value(50)))
		lgWantError(t, resp.Diagnostics, "boom", "deleted again")
		if !resp.State.Raw.IsNull() || len(fake.groups) != 0 || fake.count(http.MethodDelete, "_1") != 1 {
			t.Fatalf("%s: state null %t, groups left %d", name, resp.State.Raw.IsNull(), len(fake.groups))
		}

		// A retry is not blocked by a leftover group holding the name.
		fake.fail = nil
		resp = lgCreateGroup(t, ctx, res, lgNewGroupPlan("Engineering", types.Int64Value(50)))
		if resp.Diagnostics.HasError() || len(fake.groups) != 1 {
			t.Fatalf("%s: retry diags %v groups %d", name, resp.Diagnostics, len(fake.groups))
		}
	}
}

func TestGroupCreateRecordsTheGroupWhenRollbackFails(t *testing.T) {
	ctx := context.Background()
	fake, client := newLgFake(t, groupKindTeam)
	fake.fail = func(call lgCall) (int, string) {
		if call.Method == http.MethodPatch || call.Method == http.MethodDelete {
			return http.StatusInternalServerError, `{"error":"boom"}`
		}
		return 0, ""
	}
	res := &groupResource{client: client, kind: groupKindTeam}
	resp := lgCreateGroup(t, ctx, res, lgNewGroupPlan("Engineering", types.Int64Value(50)))
	lgWantError(t, resp.Diagnostics, "recorded in state", "untaint")
	got := lgGroup(t, ctx, res, resp.State)
	if got.ID.ValueString() != "team_group_1" || got.Name.ValueString() != "Engineering" || !got.MonthlySpendingLimitDollars.IsNull() {
		t.Fatalf("state = %#v, want the created group without a limit", got)
	}
}

func TestGroupCreateRejectsAResultThatDiffersFromThePlan(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(*lgFake){
		"name stored differently": func(f *lgFake) { f.storeName = strings.ToLower },
		"limit not applied":       func(f *lgFake) { f.ignoreLimit = true },
	}
	for name, configure := range cases {
		fake, client := newLgFake(t, groupKindTeam)
		configure(fake)
		res := &groupResource{client: client, kind: groupKindTeam}
		resp := lgCreateGroup(t, ctx, res, lgNewGroupPlan("Engineering", types.Int64Value(50)))
		lgWantError(t, resp.Diagnostics, "instead of")
		if !resp.State.Raw.IsNull() || len(fake.groups) != 0 {
			t.Fatalf("%s: state null %t, groups left %d", name, resp.State.Raw.IsNull(), len(fake.groups))
		}
	}

	fake, client := newLgFake(t, groupKindTeam)
	fake.fail = func(call lgCall) (int, string) {
		if call.Method == http.MethodPost {
			return http.StatusCreated, `{}`
		}
		return 0, ""
	}
	resp := lgCreateGroup(t, ctx, &groupResource{client: client, kind: groupKindTeam}, lgNewGroupPlan("Engineering", types.Int64Null()))
	lgWantError(t, resp.Diagnostics, "did not include the group id")
	if !resp.State.Raw.IsNull() {
		t.Fatal("a group without an id must not be stored")
	}
}

func TestGroupCreateExplainsATakenName(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		fake.addGroup("Engineering", nil)
		resp := lgCreateGroup(t, ctx, &groupResource{client: client, kind: kind}, lgNewGroupPlan("Engineering", types.Int64Null()))
		lgWantError(t, resp.Diagnostics, "HTTP 400", "already used by another active group of the "+kind.owner(), "import")
		if !resp.State.Raw.IsNull() || len(fake.groups) != 1 {
			t.Fatalf("%s: state null %t, groups %d", name, resp.State.Raw.IsNull(), len(fake.groups))
		}
	}
}

// A patch response that carries no group is not taken as the new state.
func TestGroupUpdateReadsTheGroupWhenThePatchResponseHasNone(t *testing.T) {
	ctx := context.Background()
	fake, client := newLgFake(t, groupKindTeam)
	id := fake.addGroup("Engineering", nil, "user_a")
	fake.fail = func(call lgCall) (int, string) {
		if call.Method == http.MethodPatch {
			fake.groups[id].Name = "Platform"
			return http.StatusOK, `{}`
		}
		return 0, ""
	}
	res := &groupResource{client: client, kind: groupKindTeam}
	prior := groupModel{ID: types.StringValue(id), Name: types.StringValue("Engineering"), PublicID: types.StringValue("grp_1"), MemberCount: types.Int64Value(1), MonthlySpendingLimitDollars: types.Int64Null()}
	plan := prior
	plan.Name = types.StringValue("Platform")
	resp := lgUpdateGroup(t, ctx, res, prior, plan)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if got := lgGroup(t, ctx, res, resp.State); got.ID.ValueString() != id || got.Name.ValueString() != "Platform" || got.PublicID.ValueString() != "grp_1" || got.MemberCount.ValueInt64() != 1 {
		t.Fatalf("state = %#v", got)
	}
}

func TestGroupReadReportsDriftAndRemovesAMissingGroup(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		res := &groupResource{client: client, kind: kind}
		created := lgCreateGroup(t, ctx, res, lgNewGroupPlan("Engineering", types.Int64Value(50)))
		if created.Diagnostics.HasError() {
			t.Fatal(created.Diagnostics)
		}
		id := lgGroup(t, ctx, res, created.State).ID.ValueString()

		fake.mu.Lock()
		fake.groups[id].Name = "Platform"
		fake.groups[id].MonthlySpendingLimitDollars = nil
		fake.members[id] = []string{"user_a", "user_b"}
		fake.mu.Unlock()
		read := lgReadGroup(t, ctx, res, created.State)
		if read.Diagnostics.HasError() {
			t.Fatal(read.Diagnostics)
		}
		got := lgGroup(t, ctx, res, read.State)
		if got.Name.ValueString() != "Platform" || !got.MonthlySpendingLimitDollars.IsNull() || got.MemberCount.ValueInt64() != 2 || got.PublicID.ValueString() != "grp_1" {
			t.Fatalf("%s: refreshed state = %#v", name, got)
		}

		fake.fail = func(call lgCall) (int, string) { return http.StatusInternalServerError, `{"error":"boom"}` }
		read = lgReadGroup(t, ctx, res, created.State)
		if !read.Diagnostics.HasError() || read.State.Raw.IsNull() {
			t.Fatalf("%s: a failed read must error and keep state, diags %v", name, read.Diagnostics)
		}
		fake.fail = func(call lgCall) (int, string) { return http.StatusOK, `{}` }
		read = lgReadGroup(t, ctx, res, created.State)
		if !read.Diagnostics.HasError() || read.State.Raw.IsNull() {
			t.Fatalf("%s: a response without a group must error and keep state, diags %v", name, read.Diagnostics)
		}

		fake.fail = nil
		fake.mu.Lock()
		delete(fake.groups, id)
		fake.mu.Unlock()
		read = lgReadGroup(t, ctx, res, created.State)
		if read.Diagnostics.HasError() || !read.State.Raw.IsNull() {
			t.Fatalf("%s: a deleted group must leave state, diags %v", name, read.Diagnostics)
		}
	}
}

func TestGroupUpdateSendsOnlyWhatChanged(t *testing.T) {
	ctx := context.Background()
	limit := int64(50)
	cases := []struct {
		name      string
		planName  string
		planLimit types.Int64
		wantBody  string
	}{
		{"rename", "Platform", types.Int64Value(50), `{"name":"Platform"}`},
		{"new limit", "Engineering", types.Int64Value(0), `{"monthlySpendingLimitDollars":0}`},
		{"clear limit", "Engineering", types.Int64Null(), `{"clearMonthlySpendingLimitDollars":true}`},
		{"rename and clear", "Platform", types.Int64Null(), `{"clearMonthlySpendingLimitDollars":true,"name":"Platform"}`},
	}
	for kindName, kind := range lgKinds() {
		for _, tc := range cases {
			fake, client := newLgFake(t, kind)
			id := fake.addGroup("Engineering", &limit, "user_a")
			res := &groupResource{client: client, kind: kind}
			prior := groupModel{ID: types.StringValue(id), Name: types.StringValue("Engineering"), PublicID: types.StringValue("grp_1"), MemberCount: types.Int64Value(0), MonthlySpendingLimitDollars: types.Int64Value(50)}
			plan := prior
			plan.Name, plan.MonthlySpendingLimitDollars, plan.MemberCount = types.StringValue(tc.planName), tc.planLimit, types.Int64Unknown()
			resp := lgUpdateGroup(t, ctx, res, prior, plan)
			if resp.Diagnostics.HasError() {
				t.Fatalf("%s %s: %v", kindName, tc.name, resp.Diagnostics)
			}
			writes := fake.writes()
			if len(writes) != 1 || writes[0].Method != http.MethodPatch || writes[0].Body != tc.wantBody {
				t.Fatalf("%s %s: writes = %#v", kindName, tc.name, writes)
			}
			got := lgGroup(t, ctx, res, resp.State)
			if got.Name.ValueString() != tc.planName || !got.MonthlySpendingLimitDollars.Equal(tc.planLimit) || got.ID.ValueString() != id || got.PublicID.ValueString() != "grp_1" || got.MemberCount.ValueInt64() != 1 {
				t.Fatalf("%s %s: state = %#v", kindName, tc.name, got)
			}
		}
	}
}

// Prior state can lag behind the API. Only the planned change is sent; a limit the plan never showed
// is neither cleared nor hidden.
func TestGroupUpdateDoesNotApplyAChangeThePlanDidNotShow(t *testing.T) {
	ctx := context.Background()
	limit := int64(80)
	fake, client := newLgFake(t, groupKindTeam)
	id := fake.addGroup("Engineering", &limit)
	res := &groupResource{client: client, kind: groupKindTeam}
	prior := groupModel{ID: types.StringValue(id), Name: types.StringValue("Engineering"), PublicID: types.StringValue("grp_1"), MemberCount: types.Int64Value(0), MonthlySpendingLimitDollars: types.Int64Null()}
	plan := prior
	plan.Name = types.StringValue("Platform")
	resp := lgUpdateGroup(t, ctx, res, prior, plan)
	lgWantError(t, resp.Diagnostics, "spending limit of 80 dollars instead of none")
	writes := fake.writes()
	if len(writes) != 1 || writes[0].Body != `{"name":"Platform"}` {
		t.Fatalf("writes = %#v", writes)
	}
	if got := lgGroup(t, ctx, res, resp.State); got.MonthlySpendingLimitDollars.ValueInt64() != 80 || got.Name.ValueString() != "Platform" {
		t.Fatalf("state = %#v, want what the API returned", got)
	}
}

func TestGroupUpdateFailsWhenTheChangeDidNotApply(t *testing.T) {
	ctx := context.Background()
	fake, client := newLgFake(t, groupKindOrganization)
	fake.ignoreLimit = true
	id := fake.addGroup("Engineering", nil)
	res := &groupResource{client: client, kind: groupKindOrganization}
	prior := groupModel{ID: types.StringValue(id), Name: types.StringValue("Engineering"), PublicID: types.StringValue("grp_1"), MemberCount: types.Int64Value(0), MonthlySpendingLimitDollars: types.Int64Null()}
	plan := prior
	plan.MonthlySpendingLimitDollars = types.Int64Value(25)
	resp := lgUpdateGroup(t, ctx, res, prior, plan)
	lgWantError(t, resp.Diagnostics, "spending limit of none instead of 25 dollars")
	if got := lgGroup(t, ctx, res, resp.State); !got.MonthlySpendingLimitDollars.IsNull() {
		t.Fatalf("state limit = %v, want what the API reports", got.MonthlySpendingLimitDollars)
	}

	fake.fail = func(call lgCall) (int, string) {
		if call.Method == http.MethodPatch {
			return http.StatusInternalServerError, `{"error":"boom"}`
		}
		return 0, ""
	}
	resp = lgUpdateGroup(t, ctx, res, prior, plan)
	lgWantError(t, resp.Diagnostics, "boom")
	if got := lgGroup(t, ctx, res, resp.State); !got.MonthlySpendingLimitDollars.IsNull() || got.Name.ValueString() != "Engineering" {
		t.Fatalf("a failed update must leave prior state, got %#v", got)
	}
}

func TestGroupDeleteIsIdempotentAndExplainsARefusal(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		res := &groupResource{client: client, kind: kind}
		sch := resourceSchema(t, res)
		id := fake.addGroup("Engineering", nil, "user_a")
		state := mustState(t, ctx, sch, lgGroupModel(kind, groupModel{ID: types.StringValue(id), Name: types.StringValue("Engineering"), MemberCount: types.Int64Value(1)}))

		refused := &resource.DeleteResponse{State: state}
		res.Delete(ctx, resource.DeleteRequest{State: state}, refused)
		lgWantError(t, refused.Diagnostics, "still has members", res.membersTypeName(), "SCIM")
		if len(fake.groups) != 1 {
			t.Fatalf("%s: a refused delete must leave the group", name)
		}

		fake.mu.Lock()
		fake.members[id] = nil
		fake.mu.Unlock()
		for _, attempt := range []string{"delete", "already gone"} {
			resp := &resource.DeleteResponse{State: state}
			res.Delete(ctx, resource.DeleteRequest{State: state}, resp)
			if resp.Diagnostics.HasError() || len(fake.groups) != 0 {
				t.Fatalf("%s %s: diags %v groups %d", name, attempt, resp.Diagnostics, len(fake.groups))
			}
		}
	}
}

func TestGroupImportIDFormats(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		res  resource.ResourceWithImportState
		id   string
		want string
	}{
		{&groupResource{kind: groupKindTeam}, " team_group_01k2 ", "team_group_01k2"},
		{&groupResource{kind: groupKindTeam}, "grp_01k2", "grp_01k2"},
		{&groupResource{kind: groupKindTeam}, "g_abc", ""},
		{&groupResource{kind: groupKindTeam}, "group_abc", ""},
		{&groupResource{kind: groupKindTeam}, "", ""},
		{&groupResource{kind: groupKindTeam}, "team_group_", ""},
		{&groupResource{kind: groupKindOrganization}, "g_abc", "g_abc"},
		{&groupResource{kind: groupKindOrganization}, "grp_01k2", ""},
		{&groupResource{kind: groupKindOrganization}, "team_group_01k2", ""},
		{&groupResource{kind: groupKindOrganization}, "Engineering", ""},
		{&groupMembersResource{kind: groupKindTeam}, "team_group_01k2", "team_group_01k2"},
		{&groupMembersResource{kind: groupKindTeam}, "grp_01k2", ""},
		{&groupMembersResource{kind: groupKindTeam}, "team_group_01k2 user_a", ""},
		{&groupMembersResource{kind: groupKindOrganization}, "g_abc", "g_abc"},
		{&groupMembersResource{kind: groupKindOrganization}, "grp_01k2", ""},
	}
	for _, tc := range cases {
		sch := resourceSchema(t, tc.res)
		resp := &resource.ImportStateResponse{State: lgEmptyState(ctx, sch)}
		tc.res.ImportState(ctx, resource.ImportStateRequest{ID: tc.id}, resp)
		if tc.want == "" {
			if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
				t.Errorf("%T %q: want an error and no state, got %v", tc.res, tc.id, resp.Diagnostics)
			}
			continue
		}
		if resp.Diagnostics.HasError() {
			t.Errorf("%T %q: %v", tc.res, tc.id, resp.Diagnostics)
			continue
		}
		var id types.String
		resp.State.GetAttribute(ctx, path.Root("id"), &id)
		if id.ValueString() != tc.want {
			t.Errorf("%T %q: id = %q, want %q", tc.res, tc.id, id.ValueString(), tc.want)
		}
		if _, members := tc.res.(*groupMembersResource); members {
			var groupID types.String
			resp.State.GetAttribute(ctx, path.Root("group_id"), &groupID)
			if groupID.ValueString() != tc.want {
				t.Errorf("%T %q: group_id = %q, want %q", tc.res, tc.id, groupID.ValueString(), tc.want)
			}
		}
	}
}

// After import and one read, state must equal what create leaves behind, so a plan against the
// matching configuration is empty.
func TestGroupImportThenReadMatchesCreatedState(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		_, client := newLgFake(t, kind)
		res := &groupResource{client: client, kind: kind}
		sch := resourceSchema(t, res)
		created := lgCreateGroup(t, ctx, res, lgNewGroupPlan("Engineering", types.Int64Value(50)))
		if created.Diagnostics.HasError() {
			t.Fatal(created.Diagnostics)
		}
		want := lgGroup(t, ctx, res, created.State)

		importIDs := []string{want.ID.ValueString()}
		if kind == groupKindTeam {
			importIDs = append(importIDs, want.PublicID.ValueString())
		}
		for _, importID := range importIDs {
			imported := &resource.ImportStateResponse{State: lgEmptyState(ctx, sch)}
			res.ImportState(ctx, resource.ImportStateRequest{ID: importID}, imported)
			if imported.Diagnostics.HasError() {
				t.Fatal(imported.Diagnostics)
			}
			read := lgReadGroup(t, ctx, res, imported.State)
			if read.Diagnostics.HasError() {
				t.Fatal(read.Diagnostics)
			}
			if !read.State.Raw.Equal(created.State.Raw) {
				t.Fatalf("%s import %s: state %v, want %v", name, importID, read.State.Raw, created.State.Raw)
			}
		}

		missing := "team_group_404"
		if kind == groupKindOrganization {
			missing = "g_404"
		}
		imported := &resource.ImportStateResponse{State: lgEmptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: missing}, imported)
		read := lgReadGroup(t, ctx, res, imported.State)
		if read.Diagnostics.HasError() || !read.State.Raw.IsNull() {
			t.Fatalf("%s: importing a missing group must end with no state, diags %v", name, read.Diagnostics)
		}
	}
}

func lgStringErrors(ctx context.Context, attribute schema.Attribute, value types.String) int {
	resp := &validator.StringResponse{}
	for _, v := range attribute.(schema.StringAttribute).Validators {
		v.ValidateString(ctx, validator.StringRequest{Path: path.Root("value"), ConfigValue: value}, resp)
	}
	return resp.Diagnostics.ErrorsCount()
}

func TestGroupSchemaValidatorsAndPlanModifiers(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		sch := resourceSchema(t, &groupResource{kind: kind})
		for value, wantErrors := range map[string]bool{"Engineering": false, "Platform Engineering": false, "": true, " Engineering": true, "Engineering\t": true, "\u00a0Engineering": true} {
			if got := lgStringErrors(ctx, sch.Attributes["name"], types.StringValue(value)) > 0; got != wantErrors {
				t.Errorf("%s name %q: error = %t, want %t", name, value, got, wantErrors)
			}
		}
		if lgStringErrors(ctx, sch.Attributes["name"], types.StringUnknown()) != 0 {
			t.Errorf("%s: an unknown name must wait", name)
		}
		limit := sch.Attributes["monthly_spending_limit_dollars"].(schema.Int64Attribute)
		for value, wantErrors := range map[int64]bool{0: false, 500: false, maxGroupSpendingLimitDollars: false, -1: true, maxGroupSpendingLimitDollars + 1: true} {
			resp := &validator.Int64Response{}
			for _, v := range limit.Validators {
				v.ValidateInt64(ctx, validator.Int64Request{Path: path.Root("monthly_spending_limit_dollars"), ConfigValue: types.Int64Value(value)}, resp)
			}
			if resp.Diagnostics.HasError() != wantErrors {
				t.Errorf("%s limit %d: error = %t, want %t", name, value, resp.Diagnostics.HasError(), wantErrors)
			}
		}
		for _, v := range limit.Validators {
			resp := &validator.Int64Response{}
			v.ValidateInt64(ctx, validator.Int64Request{Path: path.Root("monthly_spending_limit_dollars"), ConfigValue: types.Int64Unknown()}, resp)
			if resp.Diagnostics.HasError() {
				t.Errorf("%s: an unknown limit must wait", name)
			}
		}
		publicID, ok := sch.Attributes["public_id"].(schema.StringAttribute)
		if !ok || !publicID.Computed || len(publicID.PlanModifiers) != 1 {
			t.Fatalf("%s: public_id = %#v", name, sch.Attributes["public_id"])
		}
		if _, ok := publicID.PlanModifiers[0].(keepKnownState); !ok {
			t.Errorf("%s: public_id must keep its value across an update so dependents are not replanned", name)
		}
	}

	// A raw state that is non-null stands for an existing resource.
	existing := tfsdk.State{Raw: tftypes.NewValue(tftypes.String, "present")}
	for name, tc := range map[string]struct {
		state types.String
		want  types.String
	}{
		"known state is kept":      {types.StringValue("grp_1"), types.StringValue("grp_1")},
		"null state stays unknown": {types.StringNull(), types.StringUnknown()},
	} {
		resp := &planmodifier.StringResponse{PlanValue: types.StringUnknown()}
		keepKnownState{}.PlanModifyString(ctx, planmodifier.StringRequest{State: existing, StateValue: tc.state, PlanValue: types.StringUnknown(), ConfigValue: types.StringNull()}, resp)
		if !resp.PlanValue.Equal(tc.want) {
			t.Errorf("%s: plan = %v, want %v", name, resp.PlanValue, tc.want)
		}
	}
	resp := &planmodifier.StringResponse{PlanValue: types.StringUnknown()}
	keepKnownState{}.PlanModifyString(ctx, planmodifier.StringRequest{State: tfsdk.State{Raw: tftypes.NewValue(tftypes.String, nil)}, StateValue: types.StringNull(), PlanValue: types.StringUnknown(), ConfigValue: types.StringNull()}, resp)
	if !resp.PlanValue.IsUnknown() {
		t.Errorf("create plan = %v, want unknown", resp.PlanValue)
	}
}

func TestGroupMembersSchemaValidators(t *testing.T) {
	ctx := context.Background()
	setErrors := func(sch schema.Schema, value types.Set) int {
		resp := &validator.SetResponse{}
		for _, v := range sch.Attributes["user_ids"].(schema.SetAttribute).Validators {
			v.ValidateSet(ctx, validator.SetRequest{Path: path.Root("user_ids"), PathExpression: path.MatchRoot("user_ids"), ConfigValue: value}, resp)
		}
		return resp.Diagnostics.ErrorsCount()
	}
	set := func(values ...attr.Value) types.Set { return types.SetValueMust(types.StringType, values) }
	for name, kind := range lgKinds() {
		sch := resourceSchema(t, &groupMembersResource{kind: kind})
		for label, tc := range map[string]struct {
			value     types.Set
			wantError bool
		}{
			"ids":                {set(types.StringValue("user_a"), types.StringValue("user_b")), false},
			"empty set":          {set(), false},
			"unknown set":        {types.SetUnknown(types.StringType), false},
			"unknown element":    {set(types.StringValue("user_a"), types.StringUnknown()), false},
			"null element":       {set(types.StringValue("user_a"), types.StringNull()), true},
			"empty id":           {set(types.StringValue("")), true},
			"email":              {set(types.StringValue("a@example.com")), true},
			"surrounding spaces": {set(types.StringValue(" user_a")), true},
			"numeric id":         {set(types.StringValue("12345")), false},
		} {
			if got := setErrors(sch, tc.value) > 0; got != tc.wantError {
				t.Errorf("%s user_ids %s: error = %t, want %t", name, label, got, tc.wantError)
			}
		}
		good, bad := "team_group_01k2", []string{"grp_01k2", "g_abc", "group_abc", "", "team_group_01k2 "}
		if kind == groupKindOrganization {
			good, bad = "g_abc", []string{"grp_01k2", "team_group_01k2", "group_abc", "", "g_abc "}
		}
		if lgStringErrors(ctx, sch.Attributes["group_id"], types.StringValue(good)) != 0 || lgStringErrors(ctx, sch.Attributes["group_id"], types.StringUnknown()) != 0 {
			t.Errorf("%s: group_id %q and an unknown group_id must pass", name, good)
		}
		for _, value := range bad {
			if lgStringErrors(ctx, sch.Attributes["group_id"], types.StringValue(value)) == 0 {
				t.Errorf("%s: group_id %q must fail", name, value)
			}
		}
		groupID := sch.Attributes["group_id"].(schema.StringAttribute)
		if len(groupID.PlanModifiers) != 1 || !strings.Contains(groupID.PlanModifiers[0].Description(ctx), "destroy and recreate") {
			t.Errorf("%s: group_id must force replacement", name)
		}
	}

	if _, diags := userIDValues(ctx, types.SetUnknown(types.StringType)); !diags.HasError() {
		t.Error("an unknown set must not read as an empty member set")
	}
	if _, diags := userIDValues(ctx, set(types.StringValue("user_a"), types.StringUnknown())); !diags.HasError() {
		t.Error("a set with an unknown element must not read as a known member set")
	}
	if ids, diags := userIDValues(ctx, set(types.StringValue("user_b"), types.StringValue("user_a"))); diags.HasError() || !slices.Equal(ids, []string{"user_a", "user_b"}) {
		t.Errorf("ids = %v, diags %v", ids, diags)
	}
}

func TestGroupMembersCreateRefusesMembersItWouldNotOwn(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		id := fake.addGroup("Engineering", nil, "user_x", "user_a")
		res := &groupMembersResource{client: client, kind: kind}
		resp := lgCreateMembers(t, ctx, res, id, "user_a", "user_b")
		lgWantError(t, resp.Diagnostics, "already has 1 member(s)", "user_x", "terraform import")
		if !resp.State.Raw.IsNull() || len(fake.writes()) != 0 {
			t.Fatalf("%s: state null %t, writes %#v", name, resp.State.Raw.IsNull(), fake.writes())
		}
	}
}

func TestGroupMembersCreateAddsOnlyTheMissingMembers(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		id := fake.addGroup("Engineering", nil, "user_a")
		res := &groupMembersResource{client: client, kind: kind}
		resp := lgCreateMembers(t, ctx, res, id, "user_b", "user_a", "user_c")
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: %v", name, resp.Diagnostics)
		}
		writes := fake.writes()
		if len(writes) != 1 || !strings.HasSuffix(writes[0].Path, "/"+id+"/members/bulk-add") || writes[0].Body != `{"userIds":["user_b","user_c"]}` {
			t.Fatalf("%s: writes = %#v", name, writes)
		}
		if got := lgStateIDs(t, ctx, resp.State); !slices.Equal(got, []string{"user_a", "user_b", "user_c"}) {
			t.Fatalf("%s: user_ids = %v", name, got)
		}
		var model groupMembersModel
		resp.State.Get(ctx, &model)
		if model.ID.ValueString() != id || model.GroupID.ValueString() != id {
			t.Fatalf("%s: id = %v, group_id = %v", name, model.ID, model.GroupID)
		}
		// The count matched, so no second list call was spent on a rate-limited route.
		if lists := fake.count(http.MethodGet, "/members"); lists != 1 {
			t.Fatalf("%s: member lists = %d, want 1", name, lists)
		}
	}
}

func TestGroupMembersCreateWithEmptySetAndMissingGroup(t *testing.T) {
	ctx := context.Background()
	fake, client := newLgFake(t, groupKindTeam)
	id := fake.addGroup("Engineering", nil)
	res := &groupMembersResource{client: client, kind: groupKindTeam}
	resp := lgCreateMembers(t, ctx, res, id)
	if resp.Diagnostics.HasError() || len(fake.writes()) != 0 {
		t.Fatalf("diags %v writes %#v", resp.Diagnostics, fake.writes())
	}
	if got := lgStateIDs(t, ctx, resp.State); len(got) != 0 {
		t.Fatalf("user_ids = %v, want an empty set", got)
	}

	resp = lgCreateMembers(t, ctx, res, "team_group_404", "user_a")
	lgWantError(t, resp.Diagnostics, "does not exist")
	if !resp.State.Raw.IsNull() {
		t.Fatal("members of a missing group must not be stored")
	}
}

func TestGroupMembersCreateFailsOnIgnoredIDsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	for name, omitCounts := range map[string]bool{"with addedCount": false, "without addedCount": true} {
		for kindName, kind := range lgKinds() {
			fake, client := newLgFake(t, kind)
			fake.omitCounts = omitCounts
			fake.outsiders["user_ghost"] = true
			id := fake.addGroup("Engineering", nil, "user_a")
			res := &groupMembersResource{client: client, kind: kind}
			resp := lgCreateMembers(t, ctx, res, id, "user_a", "user_b", "user_ghost")
			lgWantError(t, resp.Diagnostics, "user_ghost", "do not belong to the "+kind.owner(), "removed again")
			if strings.Contains(fmtDiag(resp.Diagnostics), "user_b") {
				t.Fatalf("%s %s: error blames a user that was added: %s", name, kindName, fmtDiag(resp.Diagnostics))
			}
			if !resp.State.Raw.IsNull() {
				t.Fatalf("%s %s: a rolled back create must not store state", name, kindName)
			}
			if got := fake.memberIDs(id); !slices.Equal(got, []string{"user_a"}) {
				t.Fatalf("%s %s: members = %v, want only the member that was there before", name, kindName, got)
			}
		}
	}
}

func TestGroupMembersCreatePartialFailure(t *testing.T) {
	ctx := context.Background()
	users := lgUsers(150)
	failSecondAdd := func(fake *lgFake, alsoRemove bool) {
		adds := 0
		fake.fail = func(call lgCall) (int, string) {
			if strings.HasSuffix(call.Path, "/bulk-add") {
				if adds++; adds == 2 {
					return http.StatusInternalServerError, `{"error":"boom"}`
				}
			}
			if alsoRemove && strings.HasSuffix(call.Path, "/bulk-remove") {
				return http.StatusInternalServerError, `{"error":"still broken"}`
			}
			return 0, ""
		}
	}

	fake, client := newLgFake(t, groupKindTeam)
	id := fake.addGroup("Engineering", nil)
	failSecondAdd(fake, false)
	res := &groupMembersResource{client: client, kind: groupKindTeam}
	resp := lgCreateMembers(t, ctx, res, id, users...)
	lgWantError(t, resp.Diagnostics, "boom", "removed again")
	if !resp.State.Raw.IsNull() || len(fake.memberIDs(id)) != 0 {
		t.Fatalf("state null %t, members left %d", resp.State.Raw.IsNull(), len(fake.memberIDs(id)))
	}

	fake, client = newLgFake(t, groupKindTeam)
	id = fake.addGroup("Engineering", nil)
	failSecondAdd(fake, true)
	res = &groupMembersResource{client: client, kind: groupKindTeam}
	resp = lgCreateMembers(t, ctx, res, id, users...)
	lgWantError(t, resp.Diagnostics, "boom", "still broken", "recorded in state")
	if got := lgStateIDs(t, ctx, resp.State); !slices.Equal(got, users[:100]) {
		t.Fatalf("state holds %d ids, want the 100 that were added", len(got))
	}
}

func TestGroupMembersWritesOnSCIMGroupExplainThemselves(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		id := fake.addGroup("Engineering", nil, "user_a")
		fake.scim[id] = true
		res := &groupMembersResource{client: client, kind: kind}
		sch := resourceSchema(t, res)

		create := lgCreateMembers(t, ctx, res, id, "user_a", "user_b")
		lgWantError(t, create.Diagnostics, "SCIM-synced", "nothing is recorded in state")
		if !create.State.Raw.IsNull() || fake.count(http.MethodPost, "/bulk-remove") != 0 {
			t.Fatalf("%s: a rejected create must not store state or attempt a rollback", name)
		}

		update := lgUpdateMembers(t, ctx, res, id, []string{"user_a"}, []string{"user_b"})
		lgWantError(t, update.Diagnostics, "SCIM-synced")
		if got := lgStateIDs(t, ctx, update.State); !slices.Equal(got, []string{"user_a"}) {
			t.Fatalf("%s: state after rejected update = %v", name, got)
		}

		model := lgMembersModel(id, "user_a")
		state := mustState(t, ctx, sch, &model)
		destroy := &resource.DeleteResponse{State: state}
		res.Delete(ctx, resource.DeleteRequest{State: state}, destroy)
		lgWantError(t, destroy.Diagnostics, "SCIM-synced", "terraform state rm")
	}
}

func TestGroupMembersReadFollowsEveryPage(t *testing.T) {
	ctx := context.Background()
	users := lgUsers(250)
	for _, tc := range []struct {
		pagination  string
		maxPageSize int
		wantLists   int
	}{
		{"", 0, 3},
		{"", 50, 5},
		{"totalPages", 0, 3},
		{"totalPages", 50, 5},
		{"none", 0, 4},
		{"none", 50, 6},
		{"unpaged", 0, 2},
	} {
		for kindName, kind := range lgKinds() {
			fake, client := newLgFake(t, kind)
			fake.pagination, fake.maxPageSize = tc.pagination, tc.maxPageSize
			id := fake.addGroup("Engineering", nil, users...)
			res := &groupMembersResource{client: client, kind: kind}
			sch := resourceSchema(t, res)
			prior := lgMembersModel(id, "user_0000")
			state := mustState(t, ctx, sch, &prior)
			resp := &resource.ReadResponse{State: state}
			res.Read(ctx, resource.ReadRequest{State: state}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("%s %q: %v", kindName, tc.pagination, resp.Diagnostics)
			}
			if got := lgStateIDs(t, ctx, resp.State); !slices.Equal(got, users) {
				t.Fatalf("%s %q max %d: read %d of %d members", kindName, tc.pagination, tc.maxPageSize, len(got), len(users))
			}
			if lists := fake.count(http.MethodGet, "/members"); lists != tc.wantLists {
				t.Fatalf("%s %q max %d: list calls = %d, want %d", kindName, tc.pagination, tc.maxPageSize, lists, tc.wantLists)
			}
		}
	}
}

func TestGroupMembersListStopsWithAnErrorInsteadOfTruncating(t *testing.T) {
	ctx := context.Background()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = fmt.Fprintf(w, `{"members":[{"userId":"user_%d"}],"pagination":{"hasNextPage":true}}`, calls)
	}))
	defer server.Close()
	client := &apiClient{httpClient: server.Client(), adminBase: server.URL, userAgent: "test"}
	members, err := client.listGroupMembers(ctx, "Team Admin API", "team-key", "/teams/directory-groups", "team_group_1")
	if err == nil || members != nil || calls != maxAdminPages {
		t.Fatalf("members %d, err %v, calls %d", len(members), err, calls)
	}
}

func TestGroupMembersReadRemovesAMissingGroupAndKeepsStateOnFailure(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		id := fake.addGroup("Engineering", nil)
		res := &groupMembersResource{client: client, kind: kind}
		sch := resourceSchema(t, res)
		prior := lgMembersModel(id, "user_a")
		state := mustState(t, ctx, sch, &prior)

		emptied := &resource.ReadResponse{State: state}
		res.Read(ctx, resource.ReadRequest{State: state}, emptied)
		if emptied.Diagnostics.HasError() || len(lgStateIDs(t, ctx, emptied.State)) != 0 {
			t.Fatalf("%s: an emptied group must read as an empty set, diags %v", name, emptied.Diagnostics)
		}

		fake.fail = func(lgCall) (int, string) { return http.StatusInternalServerError, `{"error":"boom"}` }
		failed := &resource.ReadResponse{State: state}
		res.Read(ctx, resource.ReadRequest{State: state}, failed)
		if !failed.Diagnostics.HasError() || !slices.Equal(lgStateIDs(t, ctx, failed.State), []string{"user_a"}) {
			t.Fatalf("%s: a failed read must error and keep state, diags %v", name, failed.Diagnostics)
		}

		fake.fail = nil
		fake.mu.Lock()
		delete(fake.groups, id)
		fake.mu.Unlock()
		gone := &resource.ReadResponse{State: state}
		res.Read(ctx, resource.ReadRequest{State: state}, gone)
		if gone.Diagnostics.HasError() || !gone.State.Raw.IsNull() {
			t.Fatalf("%s: a deleted group must leave state, diags %v", name, gone.Diagnostics)
		}
	}
}

func TestGroupMembersUpdateSendsOnlyTheDelta(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		id := fake.addGroup("Engineering", nil, "user_a", "user_b", "user_c")
		res := &groupMembersResource{client: client, kind: kind}
		resp := lgUpdateMembers(t, ctx, res, id, []string{"user_a", "user_b", "user_c"}, []string{"user_b", "user_c", "user_d"})
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: %v", name, resp.Diagnostics)
		}
		writes := fake.writes()
		if len(writes) != 2 || !strings.HasSuffix(writes[0].Path, "/bulk-remove") || writes[0].Body != `{"userIds":["user_a"]}` || !strings.HasSuffix(writes[1].Path, "/bulk-add") || writes[1].Body != `{"userIds":["user_d"]}` {
			t.Fatalf("%s: writes = %#v", name, writes)
		}
		want := []string{"user_b", "user_c", "user_d"}
		if got := lgStateIDs(t, ctx, resp.State); !slices.Equal(got, want) || !slices.Equal(fake.memberIDs(id), want) {
			t.Fatalf("%s: state %v, api %v", name, got, fake.memberIDs(id))
		}
		if fake.count(http.MethodGet, "/members") != 0 {
			t.Fatalf("%s: a counted update must not list members", name)
		}

		fake.reset()
		resp = lgUpdateMembers(t, ctx, res, id, want, nil)
		if resp.Diagnostics.HasError() || len(lgStateIDs(t, ctx, resp.State)) != 0 || len(fake.memberIDs(id)) != 0 {
			t.Fatalf("%s: emptying the set left state %v api %v diags %v", name, lgStateIDs(t, ctx, resp.State), fake.memberIDs(id), resp.Diagnostics)
		}
		if writes := fake.writes(); len(writes) != 1 || !strings.HasSuffix(writes[0].Path, "/bulk-remove") {
			t.Fatalf("%s: writes = %#v", name, writes)
		}
	}
}

func TestGroupMembersUpdateChunksLargeChanges(t *testing.T) {
	ctx := context.Background()
	users := lgUsers(250)
	fake, client := newLgFake(t, groupKindTeam)
	id := fake.addGroup("Engineering", nil)
	res := &groupMembersResource{client: client, kind: groupKindTeam}
	resp := lgUpdateMembers(t, ctx, res, id, nil, users)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if adds := fake.count(http.MethodPost, "/bulk-add"); adds != 3 || !slices.Equal(fake.memberIDs(id), users) {
		t.Fatalf("adds = %d, members = %d", adds, len(fake.memberIDs(id)))
	}
}

// State can lag behind the API. An id that was already a member is not counted by addedCount and is
// not a failure.
func TestGroupMembersUpdateAcceptsAnExistingMember(t *testing.T) {
	ctx := context.Background()
	fake, client := newLgFake(t, groupKindOrganization)
	id := fake.addGroup("Engineering", nil, "user_a", "user_b")
	res := &groupMembersResource{client: client, kind: groupKindOrganization}
	resp := lgUpdateMembers(t, ctx, res, id, []string{"user_a"}, []string{"user_a", "user_b"})
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if got := lgStateIDs(t, ctx, resp.State); !slices.Equal(got, []string{"user_a", "user_b"}) || fake.count(http.MethodGet, "/members") != 1 {
		t.Fatalf("state %v, lists %d", got, fake.count(http.MethodGet, "/members"))
	}
}

func TestGroupMembersUpdateFailureRecordsTheActualMembers(t *testing.T) {
	ctx := context.Background()
	prior := []string{"user_a", "user_b"}

	fake, client := newLgFake(t, groupKindTeam)
	fake.outsiders["user_ghost"] = true
	id := fake.addGroup("Engineering", nil, prior...)
	res := &groupMembersResource{client: client, kind: groupKindTeam}
	resp := lgUpdateMembers(t, ctx, res, id, prior, []string{"user_b", "user_c", "user_ghost"})
	lgWantError(t, resp.Diagnostics, "user_ghost", "do not belong to the team", "State records the current member set")
	if got := lgStateIDs(t, ctx, resp.State); !slices.Equal(got, []string{"user_b", "user_c"}) {
		t.Fatalf("state = %v, want the members the API has", got)
	}

	fake, client = newLgFake(t, groupKindTeam)
	id = fake.addGroup("Engineering", nil, prior...)
	fake.fail = func(call lgCall) (int, string) {
		if strings.HasSuffix(call.Path, "/bulk-add") {
			return http.StatusInternalServerError, `{"error":"boom"}`
		}
		return 0, ""
	}
	res = &groupMembersResource{client: client, kind: groupKindTeam}
	resp = lgUpdateMembers(t, ctx, res, id, prior, []string{"user_b", "user_c"})
	lgWantError(t, resp.Diagnostics, "boom")
	if got := lgStateIDs(t, ctx, resp.State); !slices.Equal(got, []string{"user_b"}) {
		t.Fatalf("state after a failed add = %v, want the removal recorded", got)
	}

	fake, client = newLgFake(t, groupKindTeam)
	id = fake.addGroup("Engineering", nil, prior...)
	fake.fail = func(call lgCall) (int, string) {
		if call.Method == http.MethodPost || strings.HasSuffix(call.Path, "/members") {
			return http.StatusInternalServerError, `{"error":"boom"}`
		}
		return 0, ""
	}
	res = &groupMembersResource{client: client, kind: groupKindTeam}
	resp = lgUpdateMembers(t, ctx, res, id, prior, []string{"user_b", "user_c"})
	lgWantError(t, resp.Diagnostics, "boom")
	if got := lgStateIDs(t, ctx, resp.State); !slices.Equal(got, prior) || fake.count(http.MethodPost, "/bulk-add") != 0 {
		t.Fatalf("state after a failed remove = %v, adds = %d", got, fake.count(http.MethodPost, "/bulk-add"))
	}
}

func TestGroupMembersDeleteEmptiesTheGroupSoItCanBeDeleted(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		id := fake.addGroup("Engineering", nil, "user_a", "user_b", "user_added_elsewhere")
		members := &groupMembersResource{client: client, kind: kind}
		group := &groupResource{client: client, kind: kind}
		membersModel := lgMembersModel(id, "user_a", "user_b")
		membersState := mustState(t, ctx, resourceSchema(t, members), &membersModel)
		groupState := mustState(t, ctx, resourceSchema(t, group), lgGroupModel(kind, groupModel{ID: types.StringValue(id), Name: types.StringValue("Engineering"), MemberCount: types.Int64Value(2)}))

		resp := &resource.DeleteResponse{State: membersState}
		members.Delete(ctx, resource.DeleteRequest{State: membersState}, resp)
		if resp.Diagnostics.HasError() || len(fake.memberIDs(id)) != 0 {
			t.Fatalf("%s: diags %v, members left %v", name, resp.Diagnostics, fake.memberIDs(id))
		}
		groupResp := &resource.DeleteResponse{State: groupState}
		group.Delete(ctx, resource.DeleteRequest{State: groupState}, groupResp)
		if groupResp.Diagnostics.HasError() || len(fake.groups) != 0 {
			t.Fatalf("%s: group delete diags %v", name, groupResp.Diagnostics)
		}

		// The group is already gone, as when it was deleted first or outside Terraform.
		fake.reset()
		resp = &resource.DeleteResponse{State: membersState}
		members.Delete(ctx, resource.DeleteRequest{State: membersState}, resp)
		if resp.Diagnostics.HasError() || len(fake.writes()) != 0 {
			t.Fatalf("%s: delete after the group is gone: diags %v writes %#v", name, resp.Diagnostics, fake.writes())
		}
	}
}

func TestGroupMembersDeleteFailsWhenMembersCannotBeListed(t *testing.T) {
	ctx := context.Background()
	fake, client := newLgFake(t, groupKindTeam)
	id := fake.addGroup("Engineering", nil, "user_a")
	fake.fail = func(call lgCall) (int, string) {
		if call.Method == http.MethodGet {
			return http.StatusInternalServerError, `{"error":"boom"}`
		}
		return 0, ""
	}
	res := &groupMembersResource{client: client, kind: groupKindTeam}
	model := lgMembersModel(id, "user_a")
	state := mustState(t, ctx, resourceSchema(t, res), &model)
	resp := &resource.DeleteResponse{State: state}
	res.Delete(ctx, resource.DeleteRequest{State: state}, resp)
	if !resp.Diagnostics.HasError() || len(fake.writes()) != 0 {
		t.Fatalf("diags %v writes %#v", resp.Diagnostics, fake.writes())
	}
}

func TestGroupMembersImportThenReadMatchesCreatedState(t *testing.T) {
	ctx := context.Background()
	for name, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		id := fake.addGroup("Engineering", nil)
		res := &groupMembersResource{client: client, kind: kind}
		sch := resourceSchema(t, res)
		created := lgCreateMembers(t, ctx, res, id, "user_b", "user_a")
		if created.Diagnostics.HasError() {
			t.Fatal(created.Diagnostics)
		}

		imported := &resource.ImportStateResponse{State: lgEmptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: id}, imported)
		if imported.Diagnostics.HasError() {
			t.Fatal(imported.Diagnostics)
		}
		read := &resource.ReadResponse{State: imported.State}
		res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
		if read.Diagnostics.HasError() {
			t.Fatal(read.Diagnostics)
		}
		if !read.State.Raw.Equal(created.State.Raw) {
			t.Fatalf("%s: imported state %v, want %v", name, read.State.Raw, created.State.Raw)
		}

		// Imported values must pass the schema's own validators.
		var model groupMembersModel
		read.State.Get(ctx, &model)
		if lgStringErrors(ctx, sch.Attributes["group_id"], model.GroupID) != 0 {
			t.Fatalf("%s: imported group_id %v fails validation", name, model.GroupID)
		}
		resp := &validator.SetResponse{}
		for _, v := range sch.Attributes["user_ids"].(schema.SetAttribute).Validators {
			v.ValidateSet(ctx, validator.SetRequest{Path: path.Root("user_ids"), PathExpression: path.MatchRoot("user_ids"), ConfigValue: model.UserIDs}, resp)
		}
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: imported user_ids fail validation: %v", name, resp.Diagnostics)
		}

		missing := strings.Replace(id, "1", "404", 1)
		imported = &resource.ImportStateResponse{State: lgEmptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: missing}, imported)
		read = &resource.ReadResponse{State: imported.State}
		res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
		if read.Diagnostics.HasError() || !read.State.Raw.IsNull() {
			t.Fatalf("%s: importing members of a missing group must end with no state, diags %v", name, read.Diagnostics)
		}
	}
}

func TestGroupImportExamplesUseAcceptedIDs(t *testing.T) {
	for _, tc := range []struct {
		resource string
		kind     groupKind
		members  bool
	}{
		{"cursor_team_directory_group", groupKindTeam, false},
		{"cursor_team_directory_group_members", groupKindTeam, true},
		{"cursor_organization_group", groupKindOrganization, false},
		{"cursor_organization_group_members", groupKindOrganization, true},
	} {
		raw, err := os.ReadFile("../../examples/resources/" + tc.resource + "/import.sh")
		if err != nil {
			t.Fatal(err)
		}
		commands := 0
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 || fields[0] == "#" {
				continue
			}
			if len(fields) != 4 || fields[0] != "terraform" || fields[1] != "import" || !strings.HasPrefix(fields[2], tc.resource+".") {
				t.Fatalf("%s: unexpected import example line %q", tc.resource, line)
			}
			commands++
			if _, err := groupImportID(tc.kind, fields[3], tc.members); err != nil {
				t.Errorf("%s: %v", tc.resource, err)
			}
		}
		if commands == 0 {
			t.Errorf("%s: import example has no command", tc.resource)
		}
	}
}

// lgServer drives a resource through the provider server the way Terraform does: validate, plan
// from a proposed state, apply, and then hold the result to the plan.
type lgServer struct {
	t       *testing.T
	ctx     context.Context
	server  tfprotov6.ProviderServer
	schemas *tfprotov6.GetProviderSchemaResponse
}

func newLgServer(t *testing.T, client *apiClient) *lgServer {
	t.Helper()
	ctx := context.Background()
	server, err := providerserver.NewProtocol6WithError(mockClientProvider{Provider: New("test")(), client: client})()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	s := &lgServer{t: t, ctx: ctx, server: server, schemas: schemas}
	providerType := schemas.Provider.ValueType()
	nulls := map[string]tftypes.Value{}
	for name, typ := range providerType.(tftypes.Object).AttributeTypes {
		nulls[name] = tftypes.NewValue(typ, nil)
	}
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: s.encode(providerType, tftypes.NewValue(providerType, nulls))})
	if err != nil {
		t.Fatal(err)
	}
	s.requireNoErrors("configure", configured.Diagnostics)
	return s
}

func (s *lgServer) typ(typeName string) tftypes.Type {
	s.t.Helper()
	sch, ok := s.schemas.ResourceSchemas[typeName]
	if !ok {
		s.t.Fatalf("provider has no resource %s", typeName)
	}
	return sch.ValueType()
}

func (s *lgServer) encode(typ tftypes.Type, value tftypes.Value) *tfprotov6.DynamicValue {
	s.t.Helper()
	encoded, err := tfprotov6.NewDynamicValue(typ, value)
	if err != nil {
		s.t.Fatal(err)
	}
	return &encoded
}

func (s *lgServer) decode(typeName string, value *tfprotov6.DynamicValue) tftypes.Value {
	s.t.Helper()
	decoded, err := value.Unmarshal(s.typ(typeName))
	if err != nil {
		s.t.Fatal(err)
	}
	return decoded
}

func (s *lgServer) errors(diags []*tfprotov6.Diagnostic) string {
	var out []string
	for _, d := range diags {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			out = append(out, d.Summary+": "+d.Detail)
		}
	}
	return strings.Join(out, "; ")
}

func (s *lgServer) requireNoErrors(step string, diags []*tfprotov6.Diagnostic) {
	s.t.Helper()
	if text := s.errors(diags); text != "" {
		s.t.Fatalf("%s: %s", step, text)
	}
}

func (s *lgServer) null(typeName string) tftypes.Value {
	return tftypes.NewValue(s.typ(typeName), nil)
}

func (s *lgServer) config(typeName string, set map[string]tftypes.Value) tftypes.Value {
	typ := s.typ(typeName)
	values := map[string]tftypes.Value{}
	for name, attrType := range typ.(tftypes.Object).AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	for name, value := range set {
		values[name] = value
	}
	return tftypes.NewValue(typ, values)
}

func (s *lgServer) attrs(value tftypes.Value) map[string]tftypes.Value {
	s.t.Helper()
	var out map[string]tftypes.Value
	if err := value.As(&out); err != nil {
		s.t.Fatal(err)
	}
	// As hands back the map inside value, so callers get a copy they can edit.
	return maps.Clone(out)
}

func (s *lgServer) text(value tftypes.Value, name string) string {
	s.t.Helper()
	var out string
	if err := s.attrs(value)[name].As(&out); err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
	return out
}

func (s *lgServer) validationErrors(typeName string, config tftypes.Value) string {
	s.t.Helper()
	resp, err := s.server.ValidateResourceConfig(s.ctx, &tfprotov6.ValidateResourceConfigRequest{TypeName: typeName, Config: s.encode(s.typ(typeName), config)})
	if err != nil {
		s.t.Fatal(err)
	}
	return s.errors(resp.Diagnostics)
}

// plan builds the proposed state as Terraform does: configuration, with computed attributes that
// the configuration leaves null carried over from prior state.
func (s *lgServer) plan(typeName string, prior, config tftypes.Value) (tftypes.Value, *tfprotov6.PlanResourceChangeResponse) {
	s.t.Helper()
	proposed := config
	if !prior.IsNull() {
		priorAttrs, values := s.attrs(prior), s.attrs(config)
		for _, attribute := range s.schemas.ResourceSchemas[typeName].Block.Attributes {
			if attribute.Computed && values[attribute.Name].IsNull() {
				values[attribute.Name] = priorAttrs[attribute.Name]
			}
		}
		proposed = tftypes.NewValue(s.typ(typeName), values)
	}
	typ := s.typ(typeName)
	resp, err := s.server.PlanResourceChange(s.ctx, &tfprotov6.PlanResourceChangeRequest{TypeName: typeName, PriorState: s.encode(typ, prior), ProposedNewState: s.encode(typ, proposed), Config: s.encode(typ, config)})
	if err != nil {
		s.t.Fatal(err)
	}
	s.requireNoErrors(typeName+" plan", resp.Diagnostics)
	return s.decode(typeName, resp.PlannedState), resp
}

// apply plans and applies config, and fails the test the way Terraform reports an inconsistent
// result: every value the plan knew must be the value in the new state, and nothing may stay unknown.
func (s *lgServer) apply(typeName string, prior, config tftypes.Value) tftypes.Value {
	s.t.Helper()
	planned, plan := s.plan(typeName, prior, config)
	typ := s.typ(typeName)
	resp, err := s.server.ApplyResourceChange(s.ctx, &tfprotov6.ApplyResourceChangeRequest{TypeName: typeName, PriorState: s.encode(typ, prior), PlannedState: plan.PlannedState, PlannedPrivate: plan.PlannedPrivate, Config: s.encode(typ, config)})
	if err != nil {
		s.t.Fatal(err)
	}
	s.requireNoErrors(typeName+" apply", resp.Diagnostics)
	state := s.decode(typeName, resp.NewState)
	if !state.IsFullyKnown() {
		s.t.Fatalf("%s: state after apply has unknown values: %s", typeName, state)
	}
	applied := s.attrs(state)
	for name, want := range s.attrs(planned) {
		if want.IsFullyKnown() && !want.Equal(applied[name]) {
			s.t.Fatalf("%s.%s: planned %s, applied %s", typeName, name, want, applied[name])
		}
	}
	return state
}

func (s *lgServer) destroy(typeName string, prior tftypes.Value) {
	s.t.Helper()
	typ := s.typ(typeName)
	null := s.encode(typ, s.null(typeName))
	resp, err := s.server.ApplyResourceChange(s.ctx, &tfprotov6.ApplyResourceChangeRequest{TypeName: typeName, PriorState: s.encode(typ, prior), PlannedState: null, Config: null})
	if err != nil {
		s.t.Fatal(err)
	}
	s.requireNoErrors(typeName+" destroy", resp.Diagnostics)
}

func (s *lgServer) read(typeName string, state tftypes.Value) tftypes.Value {
	s.t.Helper()
	resp, err := s.server.ReadResource(s.ctx, &tfprotov6.ReadResourceRequest{TypeName: typeName, CurrentState: s.encode(s.typ(typeName), state)})
	if err != nil {
		s.t.Fatal(err)
	}
	s.requireNoErrors(typeName+" read", resp.Diagnostics)
	return s.decode(typeName, resp.NewState)
}

func (s *lgServer) importState(typeName, id string) tftypes.Value {
	s.t.Helper()
	resp, err := s.server.ImportResourceState(s.ctx, &tfprotov6.ImportResourceStateRequest{TypeName: typeName, ID: id})
	if err != nil {
		s.t.Fatal(err)
	}
	s.requireNoErrors(typeName+" import", resp.Diagnostics)
	if len(resp.ImportedResources) != 1 {
		s.t.Fatalf("%s import returned %d resources", typeName, len(resp.ImportedResources))
	}
	return s.decode(typeName, resp.ImportedResources[0].State)
}

func (s *lgServer) requireNoChanges(step, typeName string, state, config tftypes.Value) {
	s.t.Helper()
	planned, resp := s.plan(typeName, state, config)
	if !planned.Equal(state) || len(resp.RequiresReplace) != 0 {
		s.t.Fatalf("%s: plan is not empty: state %s, planned %s, replace %v", step, state, planned, resp.RequiresReplace)
	}
}

func TestGroupLifecycleThroughTheProviderServer(t *testing.T) {
	text := func(value string) tftypes.Value { return tftypes.NewValue(tftypes.String, value) }
	userSet := func(ids ...string) tftypes.Value {
		values := []tftypes.Value{}
		for _, id := range ids {
			values = append(values, text(id))
		}
		return tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, values)
	}
	for kindName, kind := range lgKinds() {
		fake, client := newLgFake(t, kind)
		s := newLgServer(t, client)
		groupType, membersType := "cursor_team_directory_group", "cursor_team_directory_group_members"
		if kind == groupKindOrganization {
			groupType, membersType = "cursor_organization_group", "cursor_organization_group_members"
		}

		if errs := s.validationErrors(groupType, s.config(groupType, map[string]tftypes.Value{"name": text(" Engineering")})); !strings.Contains(errs, "Invalid group name") {
			t.Fatalf("%s: a padded name passed validation: %q", kindName, errs)
		}
		if errs := s.validationErrors(groupType, s.config(groupType, map[string]tftypes.Value{"name": text("Engineering"), "monthly_spending_limit_dollars": tftypes.NewValue(tftypes.Number, int64(-1))})); errs == "" {
			t.Fatalf("%s: a negative limit passed validation", kindName)
		}
		if errs := s.validationErrors(groupType, s.config(groupType, map[string]tftypes.Value{"name": tftypes.NewValue(tftypes.String, tftypes.UnknownValue), "monthly_spending_limit_dollars": tftypes.NewValue(tftypes.Number, tftypes.UnknownValue)})); errs != "" {
			t.Fatalf("%s: unknown values must wait: %s", kindName, errs)
		}

		groupConfig := s.config(groupType, map[string]tftypes.Value{"name": text("Engineering"), "monthly_spending_limit_dollars": tftypes.NewValue(tftypes.Number, int64(50))})
		group := s.apply(groupType, s.null(groupType), groupConfig)
		groupID, publicID := s.text(group, "id"), s.text(group, "public_id")
		s.requireNoChanges(kindName+" group after create", groupType, group, groupConfig)
		if refreshed := s.read(groupType, group); !refreshed.Equal(group) {
			t.Fatalf("%s: refresh changed state from %s to %s", kindName, group, refreshed)
		}

		// A rename must not make the ids unknown, or every dependent of public_id is replanned.
		renamed := s.config(groupType, map[string]tftypes.Value{"name": text("Platform"), "monthly_spending_limit_dollars": tftypes.NewValue(tftypes.Number, int64(50))})
		planned, _ := s.plan(groupType, group, renamed)
		plannedAttrs := s.attrs(planned)
		if !plannedAttrs["id"].Equal(text(groupID)) || !plannedAttrs["public_id"].Equal(text(publicID)) || plannedAttrs["member_count"].IsKnown() {
			t.Fatalf("%s: rename plan = %s", kindName, planned)
		}
		group = s.apply(groupType, group, renamed)
		s.requireNoChanges(kindName+" group after rename", groupType, group, renamed)

		groupConfig = s.config(groupType, map[string]tftypes.Value{"name": text("Platform")})
		group = s.apply(groupType, group, groupConfig)
		if fake.groups[groupID].MonthlySpendingLimitDollars != nil || fake.groups[groupID].Name != "Platform" {
			t.Fatalf("%s: removing the limit from configuration left %#v", kindName, fake.groups[groupID])
		}
		s.requireNoChanges(kindName+" group after clearing the limit", groupType, group, groupConfig)

		for label, config := range map[string]map[string]tftypes.Value{
			"public id as group_id": {"group_id": text(publicID), "user_ids": userSet("user_a")},
			"email as user id":      {"group_id": text(groupID), "user_ids": userSet("a@example.com")},
			"null user id":          {"group_id": text(groupID), "user_ids": tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, []tftypes.Value{tftypes.NewValue(tftypes.String, nil)})},
		} {
			if s.validationErrors(membersType, s.config(membersType, config)) == "" {
				t.Fatalf("%s: %s passed validation", kindName, label)
			}
		}
		unknownMembers := s.config(membersType, map[string]tftypes.Value{
			"group_id": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
			"user_ids": tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, []tftypes.Value{text("user_a"), tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}),
		})
		if errs := s.validationErrors(membersType, unknownMembers); errs != "" {
			t.Fatalf("%s: unknown member values must wait: %s", kindName, errs)
		}
		if plannedUnknown, _ := s.plan(membersType, s.null(membersType), unknownMembers); s.attrs(plannedUnknown)["user_ids"].IsFullyKnown() {
			t.Fatalf("%s: a set with an unknown element was planned as known", kindName)
		}

		membersConfig := s.config(membersType, map[string]tftypes.Value{"group_id": text(groupID), "user_ids": userSet("user_b", "user_a")})
		members := s.apply(membersType, s.null(membersType), membersConfig)
		s.requireNoChanges(kindName+" members after create", membersType, s.read(membersType, members), membersConfig)

		otherGroup := fake.addGroup("Other", nil)
		_, moved := s.plan(membersType, members, s.config(membersType, map[string]tftypes.Value{"group_id": text(otherGroup), "user_ids": userSet("user_b", "user_a")}))
		if len(moved.RequiresReplace) != 1 || !moved.RequiresReplace[0].Equal(tftypes.NewAttributePath().WithAttributeName("group_id")) {
			t.Fatalf("%s: changing group_id must replace, got %v", kindName, moved.RequiresReplace)
		}

		membersConfig = s.config(membersType, map[string]tftypes.Value{"group_id": text(groupID), "user_ids": userSet("user_b", "user_c")})
		members = s.apply(membersType, members, membersConfig)
		if got := fake.memberIDs(groupID); !slices.Equal(got, []string{"user_b", "user_c"}) {
			t.Fatalf("%s: members after update = %v", kindName, got)
		}
		s.requireNoChanges(kindName+" members after update", membersType, s.read(membersType, members), membersConfig)

		// Refresh sees the member the group gained, and the group plan stays empty.
		group = s.read(groupType, group)
		if count := s.attrs(group)["member_count"]; !count.Equal(tftypes.NewValue(tftypes.Number, int64(2))) {
			t.Fatalf("%s: member_count = %s", kindName, count)
		}
		s.requireNoChanges(kindName+" group after members", groupType, group, groupConfig)

		importedGroup := s.read(groupType, s.importState(groupType, groupID))
		if !importedGroup.Equal(group) {
			t.Fatalf("%s: imported group %s, want %s", kindName, importedGroup, group)
		}
		s.requireNoChanges(kindName+" imported group", groupType, importedGroup, groupConfig)
		importedMembers := s.read(membersType, s.importState(membersType, groupID))
		s.requireNoChanges(kindName+" imported members", membersType, importedMembers, membersConfig)

		emptyConfig := s.config(membersType, map[string]tftypes.Value{"group_id": text(groupID), "user_ids": userSet()})
		members = s.apply(membersType, members, emptyConfig)
		s.requireNoChanges(kindName+" members after emptying", membersType, s.read(membersType, members), emptyConfig)
		members = s.apply(membersType, members, membersConfig)

		s.destroy(membersType, members)
		s.destroy(groupType, group)
		if _, left := fake.groups[groupID]; left {
			t.Fatalf("%s: destroy left the group behind", kindName)
		}
		// Destroying again, as after a partial destroy, finds nothing and succeeds.
		s.destroy(membersType, members)
		s.destroy(groupType, group)
	}
}
