package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

type lifecycleCall struct {
	Method string
	Path   string
	Body   string
}

type lifecycleServer struct {
	mu    sync.Mutex
	calls []lifecycleCall
}

// lifecycleClient serves each request from respond, which returns a status and a JSON body, and records every call.
func lifecycleClient(t *testing.T, respond func(call lifecycleCall) (int, string)) (*apiClient, *lifecycleServer) {
	t.Helper()
	rec := &lifecycleServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := lifecycleCall{Method: r.Method, Path: r.URL.Path, Body: string(raw)}
		rec.mu.Lock()
		rec.calls = append(rec.calls, call)
		rec.mu.Unlock()
		status, body := respond(call)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return &apiClient{httpClient: server.Client(), adminBase: server.URL, teamAPIKey: "team-key", userAgent: "test"}, rec
}

func (s *lifecycleServer) writes() []lifecycleCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []lifecycleCall
	for _, call := range s.calls {
		if call.Method == http.MethodGet || call.Path == "/teams/spend" {
			continue
		}
		out = append(out, call)
	}
	return out
}

func lifecycleEmptyState(ctx context.Context, sch schema.Schema) tfsdk.State {
	return tfsdk.State{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), nil)}
}

func lifecycleStrings(t *testing.T, values ...string) types.List {
	t.Helper()
	list, diags := types.ListValueFrom(context.Background(), types.StringType, values)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return list
}

const customConfigurationJSON = `{"teamId":42,"state":"custom","newProviderDefault":"enabled","newModelDefault":"disabled"}`

func TestModelAccessConfigurationCreateKeepsPlannedDefaults(t *testing.T) {
	ctx := context.Background()
	client, rec := lifecycleClient(t, func(lifecycleCall) (int, string) {
		return http.StatusOK, `{"teamId":42,"state":"custom"}`
	})
	res := &modelAccessConfigurationResource{client: client}
	sch := resourceSchema(t, res)
	plan := modelAccessConfigurationModel{
		ID: types.StringUnknown(), TeamID: types.Int64Unknown(), State: types.StringUnknown(),
		NewProviderDefault: types.StringValue("disabled"), NewModelDefault: types.StringValue("enabled"), DeletionProtection: types.BoolValue(true),
	}
	resp := &resource.CreateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	var got modelAccessConfigurationModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatal(diags)
	}
	if got.NewProviderDefault.ValueString() != "disabled" || got.NewModelDefault.ValueString() != "enabled" {
		t.Fatalf("defaults = %v / %v, want the planned values", got.NewProviderDefault, got.NewModelDefault)
	}
	if got.ID.ValueString() != "42" || got.TeamID.ValueInt64() != 42 || got.State.ValueString() != "custom" || !got.DeletionProtection.ValueBool() {
		t.Fatalf("computed attributes = %#v", got)
	}
	writes := rec.writes()
	if len(writes) != 1 || !strings.Contains(writes[0].Body, `"newProviderDefault":"disabled"`) || !strings.Contains(writes[0].Body, `"newModelDefault":"enabled"`) {
		t.Fatalf("writes = %#v", writes)
	}
}

func TestModelAccessConfigurationWriteRejectsUnappliedResult(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"policy still off":  `{"teamId":42,"state":"unrestricted"}`,
		"different default": `{"teamId":42,"state":"custom","newProviderDefault":"enabled","newModelDefault":"enabled"}`,
	} {
		client, _ := lifecycleClient(t, func(lifecycleCall) (int, string) { return http.StatusOK, body })
		res := &modelAccessConfigurationResource{client: client}
		sch := resourceSchema(t, res)
		plan := modelAccessConfigurationModel{NewProviderDefault: types.StringValue("disabled"), NewModelDefault: types.StringValue("enabled"), DeletionProtection: types.BoolValue(true)}
		resp := &resource.CreateResponse{State: lifecycleEmptyState(ctx, sch)}
		res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
		if !resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
			t.Fatalf("%s: diags %v, state null %t", name, resp.Diagnostics, resp.State.Raw.IsNull())
		}
	}
}

func TestModelAccessConfigurationReadRemovesClearedPolicy(t *testing.T) {
	ctx := context.Background()
	prior := modelAccessConfigurationModel{
		ID: types.StringValue("42"), TeamID: types.Int64Value(42), State: types.StringValue("custom"),
		NewProviderDefault: types.StringValue("enabled"), NewModelDefault: types.StringValue("disabled"), DeletionProtection: types.BoolValue(false),
	}
	cases := map[string]struct {
		status  int
		body    string
		removed bool
	}{
		"unrestricted": {http.StatusOK, `{"teamId":42,"state":"unrestricted","newProviderDefault":null,"newModelDefault":null}`, true},
		"legacy":       {http.StatusOK, `{"teamId":42,"state":"legacy"}`, true},
		"not found":    {http.StatusNotFound, `{"message":"gone"}`, true},
		"custom":       {http.StatusOK, `{"teamId":42,"state":"custom","newProviderDefault":"disabled","newModelDefault":"disabled"}`, false},
	}
	for name, tc := range cases {
		client, _ := lifecycleClient(t, func(lifecycleCall) (int, string) { return tc.status, tc.body })
		res := &modelAccessConfigurationResource{client: client}
		sch := resourceSchema(t, res)
		state := mustState(t, ctx, sch, &prior)
		resp := &resource.ReadResponse{State: state}
		res.Read(ctx, resource.ReadRequest{State: state}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: %v", name, resp.Diagnostics)
		}
		if resp.State.Raw.IsNull() != tc.removed {
			t.Fatalf("%s: removed = %t, want %t", name, resp.State.Raw.IsNull(), tc.removed)
		}
		if tc.removed {
			continue
		}
		var got modelAccessConfigurationModel
		resp.State.Get(ctx, &got)
		if got.NewProviderDefault.ValueString() != "disabled" || got.DeletionProtection.ValueBool() {
			t.Fatalf("%s: drift not recorded or deletion_protection changed: %#v", name, got)
		}
	}
}

func TestModelAccessConfigurationUpdateSkipsWriteWhenOnlyProtectionChanges(t *testing.T) {
	ctx := context.Background()
	client, rec := lifecycleClient(t, func(lifecycleCall) (int, string) { return http.StatusOK, customConfigurationJSON })
	res := &modelAccessConfigurationResource{client: client}
	sch := resourceSchema(t, res)
	prior := modelAccessConfigurationModel{
		ID: types.StringValue("42"), TeamID: types.Int64Value(42), State: types.StringValue("custom"),
		NewProviderDefault: types.StringValue("enabled"), NewModelDefault: types.StringValue("disabled"), DeletionProtection: types.BoolValue(true),
	}
	plan := prior
	plan.State = types.StringUnknown()
	plan.DeletionProtection = types.BoolValue(false)
	resp := &resource.UpdateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: mustState(t, ctx, sch, &prior)}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	var got modelAccessConfigurationModel
	resp.State.Get(ctx, &got)
	if got.DeletionProtection.ValueBool() || got.State.ValueString() != "custom" || got.ID.ValueString() != "42" {
		t.Fatalf("state = %#v", got)
	}
	if writes := rec.writes(); len(writes) != 0 {
		t.Fatalf("a deletion_protection change wrote the team policy: %#v", writes)
	}

	plan.NewModelDefault = types.StringValue("enabled")
	client, rec = lifecycleClient(t, func(lifecycleCall) (int, string) {
		return http.StatusOK, `{"teamId":42,"state":"custom","newProviderDefault":"enabled","newModelDefault":"enabled"}`
	})
	res = &modelAccessConfigurationResource{client: client}
	resp = &resource.UpdateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: mustState(t, ctx, sch, &prior)}, resp)
	if resp.Diagnostics.HasError() || len(rec.writes()) != 1 {
		t.Fatalf("diags %v writes %#v", resp.Diagnostics, rec.writes())
	}
}

func TestModelAccessConfigurationDelete(t *testing.T) {
	ctx := context.Background()
	state := modelAccessConfigurationModel{
		ID: types.StringValue("42"), TeamID: types.Int64Value(42), State: types.StringValue("custom"),
		NewProviderDefault: types.StringValue("enabled"), NewModelDefault: types.StringValue("disabled"), DeletionProtection: types.BoolValue(true),
	}
	client, rec := lifecycleClient(t, func(lifecycleCall) (int, string) { return http.StatusOK, `{"teamId":42,"state":"unrestricted"}` })
	res := &modelAccessConfigurationResource{client: client}
	sch := resourceSchema(t, res)
	resp := &resource.DeleteResponse{}
	res.Delete(ctx, resource.DeleteRequest{State: mustState(t, ctx, sch, &state)}, resp)
	if !resp.Diagnostics.HasError() || len(rec.writes()) != 0 {
		t.Fatalf("protected delete: diags %v writes %#v", resp.Diagnostics, rec.writes())
	}

	state.DeletionProtection = types.BoolValue(false)
	resp = &resource.DeleteResponse{}
	res.Delete(ctx, resource.DeleteRequest{State: mustState(t, ctx, sch, &state)}, resp)
	writes := rec.writes()
	if resp.Diagnostics.HasError() || len(writes) != 1 || writes[0].Body != `{"state":"unrestricted"}` {
		t.Fatalf("delete: diags %v writes %#v", resp.Diagnostics, writes)
	}

	client, _ = lifecycleClient(t, func(lifecycleCall) (int, string) { return http.StatusNotFound, `{"message":"gone"}` })
	res = &modelAccessConfigurationResource{client: client}
	resp = &resource.DeleteResponse{}
	res.Delete(ctx, resource.DeleteRequest{State: mustState(t, ctx, sch, &state)}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete of a missing policy: %v", resp.Diagnostics)
	}
}

func TestModelAccessConfigurationImport(t *testing.T) {
	ctx := context.Background()
	body := customConfigurationJSON
	client, _ := lifecycleClient(t, func(lifecycleCall) (int, string) { return http.StatusOK, body })
	res := &modelAccessConfigurationResource{client: client}
	sch := resourceSchema(t, res)

	for _, id := range []string{"team", "42", " 42 "} {
		imported := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: id}, imported)
		if imported.Diagnostics.HasError() {
			t.Fatalf("%q: %v", id, imported.Diagnostics)
		}
		read := &resource.ReadResponse{State: imported.State}
		res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
		if read.Diagnostics.HasError() {
			t.Fatalf("%q: %v", id, read.Diagnostics)
		}
		var got modelAccessConfigurationModel
		read.State.Get(ctx, &got)
		if got.ID.ValueString() != "42" || got.NewProviderDefault.ValueString() != "enabled" || got.NewModelDefault.ValueString() != "disabled" || !got.DeletionProtection.ValueBool() || got.State.ValueString() != "custom" {
			t.Fatalf("%q: imported state = %#v", id, got)
		}
	}

	wrongTeam := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.ImportState(ctx, resource.ImportStateRequest{ID: "7"}, wrongTeam)
	if !wrongTeam.Diagnostics.HasError() || !strings.Contains(wrongTeam.Diagnostics[0].Detail(), "does not match team 42") {
		t.Fatalf("wrong team id: %v", wrongTeam.Diagnostics)
	}

	body = `{"teamId":42,"state":"unrestricted"}`
	missing := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.ImportState(ctx, resource.ImportStateRequest{ID: "team"}, missing)
	if !missing.Diagnostics.HasError() || !strings.Contains(missing.Diagnostics[0].Detail(), "no custom policy to import") {
		t.Fatalf("import without a policy: %v", missing.Diagnostics)
	}
}

type modelAccessFake struct {
	configuration string
	conflict      bool
	providers     string
	models        string
	putStatus     int
	putBody       string
}

func (f *modelAccessFake) respond(call lifecycleCall) (int, string) {
	conflict := `{"code":"error","message":"model access is unrestricted"}`
	switch {
	case call.Path == "/teams/model-access/configuration":
		return http.StatusOK, f.configuration
	case f.conflict:
		return http.StatusConflict, conflict
	case call.Method == http.MethodPut:
		if f.putStatus != 0 {
			return f.putStatus, f.putBody
		}
		return http.StatusOK, `{}`
	case call.Path == "/teams/model-access/providers":
		return http.StatusOK, f.providers
	case strings.HasSuffix(call.Path, "/models"):
		return http.StatusOK, f.models
	}
	return http.StatusNotFound, `{"message":"no route"}`
}

func TestModelAccessProviderReadRemovesMissingEntry(t *testing.T) {
	ctx := context.Background()
	prior := modelAccessProviderModel{ID: types.StringValue("anthropic"), ProviderID: types.StringValue("anthropic"), DisplayName: types.StringValue("Anthropic"), Enabled: types.BoolValue(true)}
	cases := map[string]struct {
		fake    modelAccessFake
		removed bool
		failed  bool
	}{
		"policy cleared":       {modelAccessFake{configuration: `{"teamId":42,"state":"unrestricted"}`, conflict: true}, true, false},
		"conflict with policy": {modelAccessFake{configuration: customConfigurationJSON, conflict: true}, false, true},
		"left the catalog":     {modelAccessFake{configuration: customConfigurationJSON, providers: `{"providers":[{"id":"openai","displayName":"OpenAI","enabled":true}]}`}, true, false},
		"drift":                {modelAccessFake{configuration: customConfigurationJSON, providers: `{"providers":[{"id":"anthropic","displayName":"Anthropic","enabled":false}]}`}, false, false},
	}
	for name, tc := range cases {
		client, _ := lifecycleClient(t, tc.fake.respond)
		res := &modelAccessProviderResource{client: client}
		sch := resourceSchema(t, res)
		state := mustState(t, ctx, sch, &prior)
		resp := &resource.ReadResponse{State: state}
		res.Read(ctx, resource.ReadRequest{State: state}, resp)
		if resp.Diagnostics.HasError() != tc.failed {
			t.Fatalf("%s: diags %v", name, resp.Diagnostics)
		}
		if resp.State.Raw.IsNull() != tc.removed {
			t.Fatalf("%s: removed = %t, want %t", name, resp.State.Raw.IsNull(), tc.removed)
		}
		if name == "drift" {
			var got modelAccessProviderModel
			resp.State.Get(ctx, &got)
			if got.Enabled.ValueBool() {
				t.Fatal("refresh did not record the disabled provider")
			}
		}
	}
}

func TestModelAccessProviderWriteChecksResult(t *testing.T) {
	ctx := context.Background()
	fake := &modelAccessFake{configuration: customConfigurationJSON, providers: `{"providers":[{"id":"anthropic","displayName":"Anthropic","enabled":false}]}`}
	client, rec := lifecycleClient(t, fake.respond)
	res := &modelAccessProviderResource{client: client}
	sch := resourceSchema(t, res)
	plan := modelAccessProviderModel{ID: types.StringUnknown(), ProviderID: types.StringValue("anthropic"), DisplayName: types.StringUnknown(), Enabled: types.BoolValue(true)}
	resp := &resource.CreateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "enabled = false") || !resp.State.Raw.IsNull() {
		t.Fatalf("mismatched write: diags %v", resp.Diagnostics)
	}
	if writes := rec.writes(); len(writes) != 1 || writes[0].Path != "/teams/model-access/providers/anthropic" || writes[0].Body != `{"enabled":true}` {
		t.Fatalf("writes = %#v", writes)
	}

	fake.providers = `{"providers":[{"id":"anthropic","displayName":"Anthropic","enabled":true}]}`
	resp = &resource.CreateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	var got modelAccessProviderModel
	resp.State.Get(ctx, &got)
	if resp.Diagnostics.HasError() || got.ID.ValueString() != "anthropic" || got.DisplayName.ValueString() != "Anthropic" || !got.Enabled.ValueBool() {
		t.Fatalf("create: diags %v state %#v", resp.Diagnostics, got)
	}
}

func TestModelAccessProviderDeleteDisables(t *testing.T) {
	ctx := context.Background()
	state := modelAccessProviderModel{ID: types.StringValue("anthropic"), ProviderID: types.StringValue("anthropic"), DisplayName: types.StringValue("Anthropic"), Enabled: types.BoolValue(true)}
	cases := map[string]struct {
		fake   modelAccessFake
		failed bool
	}{
		"default enabled":      {modelAccessFake{configuration: customConfigurationJSON}, false},
		"default disabled":     {modelAccessFake{configuration: `{"teamId":42,"state":"custom","newProviderDefault":"disabled","newModelDefault":"enabled"}`}, false},
		"policy cleared":       {modelAccessFake{configuration: `{"teamId":42,"state":"unrestricted"}`, conflict: true}, false},
		"provider not found":   {modelAccessFake{configuration: customConfigurationJSON, putStatus: http.StatusNotFound, putBody: `{"message":"gone"}`}, false},
		"conflict with policy": {modelAccessFake{configuration: customConfigurationJSON, putStatus: http.StatusConflict, putBody: `{"message":"busy"}`}, true},
		"server error":         {modelAccessFake{configuration: customConfigurationJSON, putStatus: http.StatusInternalServerError, putBody: `{"message":"boom"}`}, true},
	}
	for name, tc := range cases {
		client, rec := lifecycleClient(t, tc.fake.respond)
		res := &modelAccessProviderResource{client: client}
		sch := resourceSchema(t, res)
		resp := &resource.DeleteResponse{}
		res.Delete(ctx, resource.DeleteRequest{State: mustState(t, ctx, sch, &state)}, resp)
		if resp.Diagnostics.HasError() != tc.failed {
			t.Fatalf("%s: failed = %t, diags %v", name, resp.Diagnostics.HasError(), resp.Diagnostics)
		}
		for _, write := range rec.writes() {
			if write.Body != `{"enabled":false}` {
				t.Fatalf("%s: destroy wrote %s, want only a disable", name, write.Body)
			}
		}
		if len(rec.writes()) == 0 {
			t.Fatalf("%s: destroy made no write", name)
		}
	}
}

func TestModelAccessModelUpdateClearsRemovedParameters(t *testing.T) {
	ctx := context.Background()
	fake := &modelAccessFake{
		configuration: customConfigurationJSON,
		models:        `{"models":[{"id":"claude-opus-4-6","displayName":"Opus","enabled":true,"parameters":[{"id":"fast","allowedValues":null,"configuredDefaultValue":null},{"id":"reasoning","allowedValues":["high","low"],"configuredDefaultValue":"low"}]}]}`,
	}
	client, rec := lifecycleClient(t, fake.respond)
	res := &modelAccessModelResource{client: client}
	sch := resourceSchema(t, res)
	prior := modelAccessModelModel{
		ID: types.StringValue("anthropic/claude-opus-4-6"), ProviderID: types.StringValue("anthropic"), ModelID: types.StringValue("claude-opus-4-6"),
		DisplayName: types.StringValue("Opus"), Enabled: types.BoolValue(true),
		Parameters: []modelParameterModel{
			{ID: types.StringValue("fast"), AllowedValues: lifecycleStrings(t, "off"), DefaultValue: types.StringValue("off")},
			{ID: types.StringValue("reasoning"), AllowedValues: lifecycleStrings(t, "low"), DefaultValue: types.StringNull()},
		},
	}
	plan := prior
	plan.DisplayName = types.StringUnknown()
	plan.Parameters = []modelParameterModel{{ID: types.StringValue("reasoning"), AllowedValues: lifecycleStrings(t, "low", "high"), DefaultValue: types.StringValue("low")}}
	resp := &resource.UpdateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: mustState(t, ctx, sch, &prior)}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	writes := rec.writes()
	if len(writes) != 1 || writes[0].Path != "/teams/model-access/providers/anthropic/models/claude-opus-4-6" {
		t.Fatalf("writes = %#v", writes)
	}
	if !strings.Contains(writes[0].Body, `"fast":{"allowedValues":null,"defaultValue":null}`) || !strings.Contains(writes[0].Body, `"reasoning":{"allowedValues":["low","high"],"defaultValue":"low"}`) {
		t.Fatalf("body = %s", writes[0].Body)
	}
	var got modelAccessModelModel
	resp.State.Get(ctx, &got)
	if len(got.Parameters) != 1 || !got.Parameters[0].AllowedValues.Equal(lifecycleStrings(t, "low", "high")) {
		t.Fatalf("a reordered allowed_values response changed state: %#v", got.Parameters)
	}

	plan.Parameters = nil
	payload, diags := modelWritePayload(ctx, plan, prior.Parameters)
	if diags.HasError() || len(payload["parameters"].(map[string]any)) != 2 {
		t.Fatalf("dropping parameters must clear both: %#v %v", payload, diags)
	}
	payload, _ = modelWritePayload(ctx, plan, nil)
	if _, ok := payload["parameters"]; ok {
		t.Fatalf("no parameters were ever managed: %#v", payload)
	}
}

func TestModelAccessModelDeleteDisablesAndClearsParameters(t *testing.T) {
	ctx := context.Background()
	state := modelAccessModelModel{
		ID: types.StringValue("anthropic/claude-opus-4-6"), ProviderID: types.StringValue("anthropic"), ModelID: types.StringValue("claude-opus-4-6"),
		DisplayName: types.StringValue("Opus"), Enabled: types.BoolValue(true),
		Parameters: []modelParameterModel{{ID: types.StringValue("fast"), AllowedValues: lifecycleStrings(t, "off"), DefaultValue: types.StringNull()}},
	}
	want := `{"enabled":false,"parameters":{"fast":{"allowedValues":null,"defaultValue":null}}}`
	cases := map[string]struct {
		fake   modelAccessFake
		failed bool
	}{
		"default enabled":      {modelAccessFake{configuration: `{"teamId":42,"state":"custom","newProviderDefault":"disabled","newModelDefault":"enabled"}`}, false},
		"policy cleared":       {modelAccessFake{configuration: `{"teamId":42,"state":"unrestricted"}`, conflict: true}, false},
		"model not found":      {modelAccessFake{configuration: customConfigurationJSON, putStatus: http.StatusNotFound, putBody: `{"message":"gone"}`}, false},
		"conflict with policy": {modelAccessFake{configuration: customConfigurationJSON, putStatus: http.StatusConflict, putBody: `{"message":"busy"}`}, true},
	}
	for name, tc := range cases {
		client, rec := lifecycleClient(t, tc.fake.respond)
		res := &modelAccessModelResource{client: client}
		sch := resourceSchema(t, res)
		resp := &resource.DeleteResponse{}
		res.Delete(ctx, resource.DeleteRequest{State: mustState(t, ctx, sch, &state)}, resp)
		if resp.Diagnostics.HasError() != tc.failed {
			t.Fatalf("%s: failed = %t, diags %v", name, resp.Diagnostics.HasError(), resp.Diagnostics)
		}
		writes := rec.writes()
		if len(writes) != 1 || writes[0].Body != want {
			t.Fatalf("%s: writes = %#v, want %s", name, writes, want)
		}
	}
}

func TestModelAccessModelImportRecordsRestrictedParameters(t *testing.T) {
	ctx := context.Background()
	fake := &modelAccessFake{
		configuration: customConfigurationJSON,
		models:        `{"models":[{"id":"claude-opus-4-6","displayName":"Opus","enabled":true,"parameters":[{"id":"fast","supportedValues":["on","off"],"allowedValues":null,"configuredDefaultValue":null,"catalogDefaultValue":"off"},{"id":"reasoning","allowedValues":["low"],"configuredDefaultValue":"low"}]}]}`,
	}
	client, _ := lifecycleClient(t, fake.respond)
	res := &modelAccessModelResource{client: client}
	sch := resourceSchema(t, res)
	imported := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.ImportState(ctx, resource.ImportStateRequest{ID: "anthropic/claude-opus-4-6"}, imported)
	if imported.Diagnostics.HasError() {
		t.Fatal(imported.Diagnostics)
	}
	read := &resource.ReadResponse{State: imported.State}
	res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
	if read.Diagnostics.HasError() {
		t.Fatal(read.Diagnostics)
	}
	var got modelAccessModelModel
	read.State.Get(ctx, &got)
	if got.ID.ValueString() != "anthropic/claude-opus-4-6" || !got.Enabled.ValueBool() || len(got.Parameters) != 1 {
		t.Fatalf("imported state = %#v", got)
	}
	if p := got.Parameters[0]; p.ID.ValueString() != "reasoning" || !p.AllowedValues.Equal(lifecycleStrings(t, "low")) || p.DefaultValue.ValueString() != "low" {
		t.Fatalf("imported parameter = %#v", p)
	}

	// An ordinary refresh with parameters omitted from configuration keeps them out of state.
	got.Parameters = nil
	state := mustState(t, ctx, sch, &got)
	read = &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, read)
	var refreshed modelAccessModelModel
	read.State.Get(ctx, &refreshed)
	if read.Diagnostics.HasError() || refreshed.Parameters != nil {
		t.Fatalf("refresh recorded unmanaged parameters: %#v %v", refreshed.Parameters, read.Diagnostics)
	}

	fake.models = `{"models":[{"id":"claude-opus-4-6","displayName":"Opus","enabled":false,"parameters":[{"id":"fast","allowedValues":null,"configuredDefaultValue":null}]}]}`
	read = &resource.ReadResponse{State: imported.State}
	res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
	var unrestricted modelAccessModelModel
	read.State.Get(ctx, &unrestricted)
	if read.Diagnostics.HasError() || unrestricted.Parameters != nil {
		t.Fatalf("import without restrictions must leave parameters null: %#v %v", unrestricted.Parameters, read.Diagnostics)
	}

	for _, id := range []string{"claude-opus-4-6", "anthropic/", "/claude", "anthropic/claude opus"} {
		bad := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: id}, bad)
		if !bad.Diagnostics.HasError() {
			t.Fatalf("import id %q should fail", id)
		}
	}
	provider := &modelAccessProviderResource{}
	bad := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, resourceSchema(t, provider))}
	provider.ImportState(ctx, resource.ImportStateRequest{ID: "anthropic/claude-opus-4-6"}, bad)
	if !bad.Diagnostics.HasError() {
		t.Fatal("a provider import id with a model segment should fail")
	}
}

func TestModelAccessModelReadRemovesMissingEntry(t *testing.T) {
	ctx := context.Background()
	prior := modelAccessModelModel{
		ID: types.StringValue("anthropic/claude-opus-4-6"), ProviderID: types.StringValue("anthropic"), ModelID: types.StringValue("claude-opus-4-6"),
		DisplayName: types.StringValue("Opus"), Enabled: types.BoolValue(true),
	}
	cases := map[string]struct {
		fake    modelAccessFake
		removed bool
		failed  bool
	}{
		"policy cleared":       {modelAccessFake{configuration: `{"teamId":42,"state":"legacy"}`, conflict: true}, true, false},
		"conflict with policy": {modelAccessFake{configuration: customConfigurationJSON, conflict: true}, false, true},
		"model left catalog":   {modelAccessFake{configuration: customConfigurationJSON, models: `{"models":[{"id":"other","enabled":true}]}`}, true, false},
		"provider left":        {modelAccessFake{configuration: customConfigurationJSON, models: `{"models":[]}`, providers: `{"providers":[]}`}, true, false},
	}
	for name, tc := range cases {
		client, _ := lifecycleClient(t, tc.fake.respond)
		res := &modelAccessModelResource{client: client}
		sch := resourceSchema(t, res)
		state := mustState(t, ctx, sch, &prior)
		resp := &resource.ReadResponse{State: state}
		res.Read(ctx, resource.ReadRequest{State: state}, resp)
		if resp.Diagnostics.HasError() != tc.failed || resp.State.Raw.IsNull() != tc.removed {
			t.Fatalf("%s: diags %v removed %t", name, resp.Diagnostics, resp.State.Raw.IsNull())
		}
	}
}

func TestModelAccessModelValidateConfigRejectsDuplicateParameters(t *testing.T) {
	ctx := context.Background()
	res := &modelAccessModelResource{}
	sch := resourceSchema(t, res)
	model := modelAccessModelModel{
		ProviderID: types.StringValue("anthropic"), ModelID: types.StringValue("claude-opus-4-6"), Enabled: types.BoolValue(true),
		Parameters: []modelParameterModel{
			{ID: types.StringValue("fast"), AllowedValues: types.ListNull(types.StringType)},
			{ID: types.StringValue("fast"), AllowedValues: lifecycleStrings(t, "off")},
		},
	}
	resp := &resource.ValidateConfigResponse{}
	res.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: mustConfig(t, ctx, sch, &model)}, resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Summary(), "Duplicate parameter id") {
		t.Fatalf("duplicate ids: %v", resp.Diagnostics)
	}

	model.Parameters[1].ID = types.StringUnknown()
	resp = &resource.ValidateConfigResponse{}
	res.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: mustConfig(t, ctx, sch, &model)}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("an unknown id must wait for apply: %v", resp.Diagnostics)
	}

	unknown := tfsdk.Config{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), tftypes.UnknownValue)}
	resp = &resource.ValidateConfigResponse{}
	res.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: unknown}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("unknown parameters must wait for apply: %v", resp.Diagnostics)
	}
}

func stringValidatorErrors(ctx context.Context, validators []validator.String, value string) int {
	count := 0
	for _, v := range validators {
		resp := &validator.StringResponse{}
		v.ValidateString(ctx, validator.StringRequest{Path: path.Root("test"), ConfigValue: types.StringValue(value)}, resp)
		count += resp.Diagnostics.ErrorsCount()
	}
	return count
}

func listValidatorErrors(ctx context.Context, validators []validator.List, value types.List) int {
	count := 0
	for _, v := range validators {
		resp := &validator.ListResponse{}
		v.ValidateList(ctx, validator.ListRequest{Path: path.Root("test"), ConfigValue: value}, resp)
		count += resp.Diagnostics.ErrorsCount()
	}
	return count
}

func TestModelAccessSpendAndBlocklistValidators(t *testing.T) {
	ctx := context.Background()
	attrs := func(res resource.Resource) map[string]schema.Attribute { return resourceSchema(t, res).Attributes }

	stringCases := []struct {
		res   resource.Resource
		name  string
		value string
		valid bool
	}{
		{&modelAccessConfigurationResource{}, "new_provider_default", "enabled", true},
		{&modelAccessConfigurationResource{}, "new_model_default", "on", false},
		{&modelAccessProviderResource{}, "provider_id", "anthropic", true},
		{&modelAccessProviderResource{}, "provider_id", "", false},
		{&modelAccessProviderResource{}, "provider_id", "anthropic/claude", false},
		{&modelAccessModelResource{}, "provider_id", "open ai", false},
		{&modelAccessModelResource{}, "model_id", "claude-opus-4-6", true},
		{&modelAccessModelResource{}, "model_id", "", false},
		{&userSpendLimitResource{}, "user_email", "Owner@Example.com", true},
		{&userSpendLimitResource{}, "user_email", "owner", false},
		{&userSpendLimitResource{}, "user_email", " owner@example.com", false},
		{&userSpendLimitResource{}, "user_email", "a@b@c", false},
		{&repoBlocklistResource{}, "url", "https://github.com/acme/api", true},
		{&repoBlocklistResource{}, "url", "", false},
		{&repoBlocklistResource{}, "url", "https://github.com/acme/api ", false},
	}
	for _, tc := range stringCases {
		attribute := attrs(tc.res)[tc.name].(schema.StringAttribute)
		if got := stringValidatorErrors(ctx, attribute.Validators, tc.value) == 0; got != tc.valid {
			t.Errorf("%T.%s %q: valid = %t, want %t", tc.res, tc.name, tc.value, got, tc.valid)
		}
	}

	limit := attrs(&userSpendLimitResource{})["spend_limit_dollars"].(schema.Int64Attribute)
	for value, valid := range map[int64]bool{-1: false, 0: true, 250: true} {
		errors := 0
		for _, v := range limit.Validators {
			resp := &validator.Int64Response{}
			v.ValidateInt64(ctx, validator.Int64Request{Path: path.Root("spend_limit_dollars"), ConfigValue: types.Int64Value(value)}, resp)
			errors += resp.Diagnostics.ErrorsCount()
		}
		if (errors == 0) != valid {
			t.Errorf("spend_limit_dollars %d: valid = %t, want %t", value, errors == 0, valid)
		}
	}

	patterns := attrs(&repoBlocklistResource{})["patterns"].(schema.ListAttribute)
	for name, tc := range map[string]struct {
		value types.List
		valid bool
	}{
		"one pattern": {lifecycleStrings(t, "*.env"), true},
		"empty list":  {types.ListValueMust(types.StringType, []attr.Value{}), false},
		"duplicate":   {lifecycleStrings(t, "*.env", "*.env"), false},
		"blank":       {lifecycleStrings(t, ""), false},
		"unknown":     {types.ListUnknown(types.StringType), true},
	} {
		if got := listValidatorErrors(ctx, patterns.Validators, tc.value) == 0; got != tc.valid {
			t.Errorf("patterns %s: valid = %t, want %t", name, got, tc.valid)
		}
	}
}

type spendFake struct {
	members    string
	setOutcome string
}

func (f *spendFake) respond(call lifecycleCall) (int, string) {
	switch call.Path {
	case "/teams/spend":
		return http.StatusOK, fmt.Sprintf(`{"teamMemberSpend":[%s],"totalPages":1}`, f.members)
	case "/teams/user-spend-limit":
		if f.setOutcome != "" {
			return http.StatusOK, f.setOutcome
		}
		return http.StatusOK, `{"outcome":"success"}`
	}
	return http.StatusNotFound, `{"message":"no route"}`
}

func TestUserSpendLimitCreateRefusesUnmanagedLimit(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		members string
		planned types.Int64
		failed  string
		written bool
	}{
		"different limit":       {`{"email":"a@acme.com","monthlyLimitDollars":50}`, types.Int64Value(100), "terraform import", false},
		"limit where none":      {`{"email":"a@acme.com","monthlyLimitDollars":50}`, types.Int64Null(), "terraform import", false},
		"not a member":          {`{"email":"b@acme.com","monthlyLimitDollars":null}`, types.Int64Value(100), "not in the team's spend data", false},
		"same limit is adopted": {`{"email":"A@Acme.com","monthlyLimitDollars":100}`, types.Int64Value(100), "", true},
	}
	for name, tc := range cases {
		fake := &spendFake{members: tc.members}
		client, rec := lifecycleClient(t, fake.respond)
		res := &userSpendLimitResource{client: client}
		sch := resourceSchema(t, res)
		plan := userSpendLimitModel{ID: types.StringUnknown(), UserEmail: types.StringValue("a@acme.com"), SpendLimitDollars: tc.planned}
		resp := &resource.CreateResponse{State: lifecycleEmptyState(ctx, sch)}
		res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
		if tc.failed == "" {
			var got userSpendLimitModel
			resp.State.Get(ctx, &got)
			if resp.Diagnostics.HasError() || got.SpendLimitDollars.ValueInt64() != 100 || got.ID.ValueString() != "a@acme.com" {
				t.Fatalf("%s: diags %v state %#v", name, resp.Diagnostics, got)
			}
		} else if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), tc.failed) || !resp.State.Raw.IsNull() {
			t.Fatalf("%s: diags %v", name, resp.Diagnostics)
		}
		if written := len(rec.writes()) > 0; written != tc.written {
			t.Fatalf("%s: wrote = %t, want %t", name, written, tc.written)
		}
	}
}

func TestUserSpendLimitWriteChecksResult(t *testing.T) {
	ctx := context.Background()
	fake := &spendFake{members: `{"email":"a@acme.com","monthlyLimitDollars":null}`}
	client, _ := lifecycleClient(t, fake.respond)
	res := &userSpendLimitResource{client: client}
	sch := resourceSchema(t, res)
	prior := userSpendLimitModel{ID: types.StringValue("a@acme.com"), UserEmail: types.StringValue("a@acme.com"), SpendLimitDollars: types.Int64Null()}
	plan := prior
	plan.SpendLimitDollars = types.Int64Value(25)
	priorState := mustState(t, ctx, sch, &prior)
	resp := &resource.UpdateResponse{State: priorState}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: priorState}, resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "after writing 25 dollars") {
		t.Fatalf("mismatched write: %v", resp.Diagnostics)
	}
	var kept userSpendLimitModel
	resp.State.Get(ctx, &kept)
	if !kept.SpendLimitDollars.IsNull() {
		t.Fatalf("a failed update changed state: %#v", kept)
	}
}

func TestUserSpendLimitEmailCase(t *testing.T) {
	ctx := context.Background()
	replace := func(state, plan types.String) bool {
		resp := &stringplanmodifier.RequiresReplaceIfFuncResponse{}
		replaceUnlessSameEmail(ctx, planmodifier.StringRequest{StateValue: state, PlanValue: plan}, resp)
		return resp.RequiresReplace
	}
	if replace(types.StringValue("Owner@Example.com"), types.StringValue("owner@example.com")) {
		t.Fatal("a change of letter case must not replace the resource")
	}
	if !replace(types.StringValue("owner@example.com"), types.StringValue("other@example.com")) {
		t.Fatal("a different member must replace the resource")
	}
	if !replace(types.StringValue("owner@example.com"), types.StringUnknown()) {
		t.Fatal("an unknown email must be planned as a replacement")
	}

	fake := &spendFake{members: `{"email":"owner@example.com","monthlyLimitDollars":10}`}
	client, rec := lifecycleClient(t, fake.respond)
	res := &userSpendLimitResource{client: client}
	sch := resourceSchema(t, res)

	imported := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.ImportState(ctx, resource.ImportStateRequest{ID: " Owner@Example.com "}, imported)
	if imported.Diagnostics.HasError() {
		t.Fatal(imported.Diagnostics)
	}
	read := &resource.ReadResponse{State: imported.State}
	res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
	var state userSpendLimitModel
	read.State.Get(ctx, &state)
	if read.Diagnostics.HasError() || state.UserEmail.ValueString() != "Owner@Example.com" || state.ID.ValueString() != "owner@example.com" || state.SpendLimitDollars.ValueInt64() != 10 {
		t.Fatalf("imported state = %#v diags %v", state, read.Diagnostics)
	}

	// The id planned from state has to survive an in-place respelling of the email.
	plan := state
	plan.UserEmail = types.StringValue("owner@example.com")
	update := &resource.UpdateResponse{State: read.State}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: read.State}, update)
	var updated userSpendLimitModel
	update.State.Get(ctx, &updated)
	if update.Diagnostics.HasError() || updated.ID.ValueString() != state.ID.ValueString() || updated.UserEmail.ValueString() != "owner@example.com" {
		t.Fatalf("respelled email: %#v diags %v", updated, update.Diagnostics)
	}
	if writes := rec.writes(); len(writes) != 1 || !strings.Contains(writes[0].Body, `"spendLimitDollars":10`) {
		t.Fatalf("writes = %#v", writes)
	}

	for _, id := range []string{"owner", "", "a b@example.com"} {
		bad := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: id}, bad)
		if !bad.Diagnostics.HasError() {
			t.Fatalf("import id %q should fail", id)
		}
	}

	fake.members = ""
	gone := &resource.ReadResponse{State: read.State}
	res.Read(ctx, resource.ReadRequest{State: read.State}, gone)
	if gone.Diagnostics.HasError() || !gone.State.Raw.IsNull() {
		t.Fatalf("a member missing from spend data must leave state: %v", gone.Diagnostics)
	}
}

func TestUserSpendLimitDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	state := userSpendLimitModel{ID: types.StringValue("a@acme.com"), UserEmail: types.StringValue("a@acme.com"), SpendLimitDollars: types.Int64Value(10)}
	failure := `{"outcome":"error","message":"user is not a team member"}`
	cases := map[string]struct {
		fake   spendFake
		failed bool
	}{
		"cleared":               {spendFake{members: `{"email":"a@acme.com","monthlyLimitDollars":null}`}, false},
		"member left the team":  {spendFake{members: ``, setOutcome: failure}, false},
		"limit already cleared": {spendFake{members: `{"email":"a@acme.com","monthlyLimitDollars":null}`, setOutcome: failure}, false},
		"limit still present":   {spendFake{members: `{"email":"a@acme.com","monthlyLimitDollars":10}`, setOutcome: failure}, true},
	}
	for name, tc := range cases {
		client, rec := lifecycleClient(t, tc.fake.respond)
		res := &userSpendLimitResource{client: client}
		resp := &resource.DeleteResponse{}
		res.Delete(ctx, resource.DeleteRequest{State: mustState(t, ctx, resourceSchema(t, res), &state)}, resp)
		if resp.Diagnostics.HasError() != tc.failed {
			t.Fatalf("%s: %v", name, resp.Diagnostics)
		}
		if writes := rec.writes(); len(writes) != 1 || !strings.Contains(writes[0].Body, `"spendLimitDollars":null`) {
			t.Fatalf("%s: writes = %#v", name, writes)
		}
	}
}

type blocklistFake struct {
	repos        string
	upsert       string
	deleteStatus int
}

func (f *blocklistFake) respond(call lifecycleCall) (int, string) {
	switch {
	case call.Method == http.MethodGet && call.Path == "/settings/repo-blocklists/repos":
		return http.StatusOK, fmt.Sprintf(`{"repos":[%s]}`, f.repos)
	case call.Method == http.MethodPost && call.Path == "/settings/repo-blocklists/repos/upsert":
		return http.StatusOK, fmt.Sprintf(`{"repos":[%s]}`, f.upsert)
	case call.Method == http.MethodDelete:
		if f.deleteStatus != 0 {
			return f.deleteStatus, `{"message":"gone"}`
		}
		return http.StatusNoContent, ``
	}
	return http.StatusNotFound, `{"message":"no route"}`
}

func TestRepoBlocklistCreateRefusesExistingEntry(t *testing.T) {
	ctx := context.Background()
	fake := &blocklistFake{
		repos:  `{"id":"repo_other","url":"https://github.com/acme/web","patterns":["*"]},{"id":"repo_1","url":"https://github.com/Acme/API","patterns":["secrets/*"]}`,
		upsert: `{"id":"repo_2","url":"https://github.com/acme/api","patterns":["*.env"]}`,
	}
	client, rec := lifecycleClient(t, fake.respond)
	res := &repoBlocklistResource{client: client}
	sch := resourceSchema(t, res)
	plan := repoBlocklistModel{ID: types.StringUnknown(), URL: types.StringValue("github.com/acme/api.git"), Patterns: lifecycleStrings(t, "*.env")}
	resp := &resource.CreateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	if !resp.Diagnostics.HasError() || !strings.Contains(resp.Diagnostics[0].Detail(), "terraform import <address> repo_1") || !resp.State.Raw.IsNull() {
		t.Fatalf("existing entry: %v", resp.Diagnostics)
	}
	if writes := rec.writes(); len(writes) != 0 {
		t.Fatalf("create replaced an unmanaged entry: %#v", writes)
	}

	// The response lists every repository and spells the URL its own way, and reorders the patterns.
	fake.repos = `{"id":"repo_other","url":"https://github.com/acme/web","patterns":["*"]}`
	fake.upsert = `{"id":"repo_other","url":"https://github.com/acme/web","patterns":["*"]},{"id":"repo_2","url":"https://github.com/acme/api","patterns":["b","a"]}`
	plan.Patterns = lifecycleStrings(t, "a", "b")
	resp = &resource.CreateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.Create(ctx, resource.CreateRequest{Plan: mustPlan(t, ctx, sch, &plan)}, resp)
	var got repoBlocklistModel
	resp.State.Get(ctx, &got)
	if resp.Diagnostics.HasError() || got.ID.ValueString() != "repo_2" || got.URL.ValueString() != "github.com/acme/api.git" || !got.Patterns.Equal(plan.Patterns) {
		t.Fatalf("create: diags %v state %#v", resp.Diagnostics, got)
	}
	if writes := rec.writes(); len(writes) != 1 || writes[0].Body != `{"repos":[{"patterns":["a","b"],"url":"github.com/acme/api.git"}]}` {
		t.Fatalf("writes = %#v", writes)
	}
}

func TestRepoBlocklistReadFollowsURLAndRemovesMissing(t *testing.T) {
	ctx := context.Background()
	fake := &blocklistFake{repos: `{"id":"repo_new","url":"https://github.com/acme/api","patterns":["*.pem","*.env"]}`}
	client, _ := lifecycleClient(t, fake.respond)
	res := &repoBlocklistResource{client: client}
	sch := resourceSchema(t, res)
	prior := repoBlocklistModel{ID: types.StringValue("repo_old"), URL: types.StringValue("https://github.com/acme/api.git"), Patterns: lifecycleStrings(t, "*.env", "*.pem")}
	state := mustState(t, ctx, sch, &prior)
	resp := &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, resp)
	var got repoBlocklistModel
	resp.State.Get(ctx, &got)
	if resp.Diagnostics.HasError() || got.ID.ValueString() != "repo_new" || got.URL.ValueString() != "https://github.com/acme/api.git" || !got.Patterns.Equal(prior.Patterns) {
		t.Fatalf("recreated entry: diags %v state %#v", resp.Diagnostics, got)
	}

	fake.repos = `{"id":"repo_old","url":"https://github.com/acme/api","patterns":["*.key"]}`
	resp = &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, resp)
	resp.State.Get(ctx, &got)
	if resp.Diagnostics.HasError() || !got.Patterns.Equal(lifecycleStrings(t, "*.key")) {
		t.Fatalf("pattern drift: diags %v state %#v", resp.Diagnostics, got)
	}

	fake.repos = `{"id":"repo_x","url":"https://github.com/acme/web","patterns":["*"]}`
	resp = &resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, resp)
	if resp.Diagnostics.HasError() || !resp.State.Raw.IsNull() {
		t.Fatalf("a deleted entry must leave state: %v", resp.Diagnostics)
	}
}

func TestRepoBlocklistImportAndRespelledURL(t *testing.T) {
	ctx := context.Background()
	fake := &blocklistFake{
		repos:  `{"id":"repo_1","url":"https://github.com/acme/api","patterns":["*.env"]}`,
		upsert: `{"id":"repo_1","url":"https://github.com/acme/api","patterns":["*.env","*.pem"]}`,
	}
	client, rec := lifecycleClient(t, fake.respond)
	res := &repoBlocklistResource{client: client}
	sch := resourceSchema(t, res)

	var state tfsdk.State
	for _, id := range []string{"repo_1", "github.com/Acme/api", " https://github.com/acme/api.git "} {
		imported := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: id}, imported)
		if imported.Diagnostics.HasError() {
			t.Fatalf("%q: %v", id, imported.Diagnostics)
		}
		read := &resource.ReadResponse{State: imported.State}
		res.Read(ctx, resource.ReadRequest{State: imported.State}, read)
		var got repoBlocklistModel
		read.State.Get(ctx, &got)
		if read.Diagnostics.HasError() || got.ID.ValueString() != "repo_1" || !got.Patterns.Equal(lifecycleStrings(t, "*.env")) {
			t.Fatalf("%q: diags %v state %#v", id, read.Diagnostics, got)
		}
		if id == "repo_1" {
			if got.URL.ValueString() != "https://github.com/acme/api" {
				t.Fatalf("import by id: url = %q", got.URL.ValueString())
			}
			state = read.State
		}
	}
	for _, id := range []string{"", "  ", "two words"} {
		bad := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, sch)}
		res.ImportState(ctx, resource.ImportStateRequest{ID: id}, bad)
		if !bad.Diagnostics.HasError() {
			t.Fatalf("import id %q should fail", id)
		}
	}
	missing := &resource.ImportStateResponse{State: lifecycleEmptyState(ctx, sch)}
	res.ImportState(ctx, resource.ImportStateRequest{ID: "repo_missing"}, missing)
	read := &resource.ReadResponse{State: missing.State}
	res.Read(ctx, resource.ReadRequest{State: missing.State}, read)
	if read.Diagnostics.HasError() || !read.State.Raw.IsNull() {
		t.Fatalf("an unknown id must read as absent: %v", read.Diagnostics)
	}

	replace := func(prior, planned types.String) bool {
		resp := &stringplanmodifier.RequiresReplaceIfFuncResponse{}
		replaceUnlessSameRepo(ctx, planmodifier.StringRequest{StateValue: prior, PlanValue: planned}, resp)
		return resp.RequiresReplace
	}
	if replace(types.StringValue("https://github.com/acme/api"), types.StringValue("git@github.com:Acme/api.git")) {
		t.Fatal("another spelling of the same repository must not replace the resource")
	}
	if !replace(types.StringValue("https://github.com/acme/api"), types.StringValue("https://github.com/acme/web")) {
		t.Fatal("a different repository must replace the resource")
	}
	if !replace(types.StringValue("https://github.com/acme/api"), types.StringUnknown()) {
		t.Fatal("an unknown URL must be planned as a replacement")
	}

	plan := repoBlocklistModel{ID: types.StringValue("repo_1"), URL: types.StringValue("git@github.com:Acme/api.git"), Patterns: lifecycleStrings(t, "*.env", "*.pem")}
	update := &resource.UpdateResponse{State: state}
	res.Update(ctx, resource.UpdateRequest{Plan: mustPlan(t, ctx, sch, &plan), State: state}, update)
	var updated repoBlocklistModel
	update.State.Get(ctx, &updated)
	if update.Diagnostics.HasError() || updated.ID.ValueString() != "repo_1" || updated.URL.ValueString() != "git@github.com:Acme/api.git" || !updated.Patterns.Equal(plan.Patterns) {
		t.Fatalf("update: diags %v state %#v", update.Diagnostics, updated)
	}
	if writes := rec.writes(); len(writes) != 1 || !strings.Contains(writes[0].Body, `"url":"https://github.com/acme/api"`) {
		t.Fatalf("the upsert must address the entry by the spelling in state: %#v", writes)
	}
}

func TestRepoBlocklistDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	state := repoBlocklistModel{ID: types.StringValue("repo_1"), URL: types.StringValue("https://github.com/acme/api"), Patterns: lifecycleStrings(t, "*.env")}
	for status, failed := range map[int]bool{http.StatusNoContent: false, http.StatusNotFound: false, http.StatusInternalServerError: true} {
		fake := &blocklistFake{deleteStatus: status}
		client, rec := lifecycleClient(t, fake.respond)
		res := &repoBlocklistResource{client: client}
		resp := &resource.DeleteResponse{}
		res.Delete(ctx, resource.DeleteRequest{State: mustState(t, ctx, resourceSchema(t, res), &state)}, resp)
		if resp.Diagnostics.HasError() != failed {
			t.Fatalf("status %d: %v", status, resp.Diagnostics)
		}
		if writes := rec.writes(); len(writes) != 1 || writes[0].Method != http.MethodDelete || writes[0].Path != "/settings/repo-blocklists/repos/repo_1" {
			t.Fatalf("status %d: writes = %#v", status, writes)
		}
	}
}
