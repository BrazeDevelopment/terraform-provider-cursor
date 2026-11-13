package provider

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cursor/terraform-provider-cursor/internal/publicapi"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	pschema "github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"
)

var ldsEnvKeys = []string{envToken, envEndpoint, envAPIKey, envTeamAPIKey, envOrgAPIKey, envPoolAPIKey, envOrgID}

func ldsClearEnv(t *testing.T) {
	t.Helper()
	for _, key := range ldsEnvKeys {
		t.Setenv(key, "")
	}
}

func ldsProviderSchema(t *testing.T) (*cursorProvider, pschema.Schema) {
	t.Helper()
	p := &cursorProvider{version: "test"}
	resp := &provider.SchemaResponse{}
	p.Schema(context.Background(), provider.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return p, resp.Schema
}

// ldsObject builds an object of the given type in which every attribute is null
// except the ones in values. Unlike a model struct it can hold unknown values.
func ldsObject(t *testing.T, typ tftypes.Type, values map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	objType, ok := typ.(tftypes.Object)
	if !ok {
		t.Fatalf("%s is not an object type", typ)
	}
	attrs := make(map[string]tftypes.Value, len(objType.AttributeTypes))
	for name, attrType := range objType.AttributeTypes {
		attrs[name] = tftypes.NewValue(attrType, nil)
	}
	for name, value := range values {
		if _, ok := objType.AttributeTypes[name]; !ok {
			t.Fatalf("no attribute %q in %s", name, typ)
		}
		attrs[name] = value
	}
	return tftypes.NewValue(objType, attrs)
}

func ldsString(value string) tftypes.Value { return tftypes.NewValue(tftypes.String, value) }

func ldsConfigure(t *testing.T, p *cursorProvider, sch pschema.Schema, deferralAllowed bool, values map[string]tftypes.Value) *provider.ConfigureResponse {
	t.Helper()
	ctx := context.Background()
	resp := &provider.ConfigureResponse{}
	p.Configure(ctx, provider.ConfigureRequest{
		Config:             tfsdk.Config{Schema: sch, Raw: ldsObject(t, sch.Type().TerraformType(ctx), values)},
		ClientCapabilities: provider.ConfigureProviderClientCapabilities{DeferralAllowed: deferralAllowed},
	}, resp)
	return resp
}

func ldsConfiguredClient(t *testing.T, p *cursorProvider, sch pschema.Schema, values map[string]tftypes.Value) *apiClient {
	t.Helper()
	resp := ldsConfigure(t, p, sch, false, values)
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 0 {
		t.Fatalf("configure diagnostics: %v", resp.Diagnostics)
	}
	client, ok := resp.ResourceData.(*apiClient)
	if !ok || resp.DataSourceData != resp.ResourceData {
		t.Fatalf("resource data = %T, data source data = %T", resp.ResourceData, resp.DataSourceData)
	}
	return client
}

func TestConfigureDefersWhenAnArgumentIsUnknown(t *testing.T) {
	ctx := context.Background()
	p, sch := ldsProviderSchema(t)
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = io.WriteString(w, `{"accessToken":"session"}`)
	}))
	defer server.Close()

	guardrailsType := sch.Type().TerraformType(ctx).(tftypes.Object).AttributeTypes["guardrails"]
	listType := tftypes.List{ElementType: tftypes.String}
	unknownString := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	cases := map[string]struct {
		argument string
		value    tftypes.Value
	}{
		"token":                {"token", unknownString},
		"endpoint":             {"endpoint", unknownString},
		"api_key":              {"api_key", unknownString},
		"team_api_key":         {"team_api_key", unknownString},
		"organization_api_key": {"organization_api_key", unknownString},
		"organization_id":      {"organization_id", unknownString},
		"pool_api_key":         {"pool_api_key", unknownString},
		"guardrails block":     {"guardrails", tftypes.NewValue(guardrailsType, tftypes.UnknownValue)},
		"guardrails bool": {"guardrails", ldsObject(t, guardrailsType, map[string]tftypes.Value{
			"require_pool_label": tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue),
		})},
		"guardrails list": {"guardrails", ldsObject(t, guardrailsType, map[string]tftypes.Value{
			"allowed_models": tftypes.NewValue(listType, tftypes.UnknownValue),
		})},
		"guardrails list element": {"guardrails", ldsObject(t, guardrailsType, map[string]tftypes.Value{
			"allowed_repo_owners": tftypes.NewValue(listType, []tftypes.Value{ldsString("acme"), unknownString}),
		})},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// A raw key in the environment would be exchanged over the network, and any
			// environment value would be a different credential from the configured one.
			t.Setenv(envToken, "key_from_environment")
			t.Setenv(envEndpoint, server.URL)
			t.Setenv(envAPIKey, "key_from_environment")
			t.Setenv(envTeamAPIKey, "team_from_environment")
			t.Setenv(envOrgAPIKey, "org_from_environment")
			t.Setenv(envPoolAPIKey, "pool_from_environment")
			t.Setenv(envOrgID, "org_environment")
			values := map[string]tftypes.Value{
				"token":    ldsString("key_configured"),
				"endpoint": ldsString(server.URL),
				"api_key":  ldsString("key_configured"),
			}
			values[tc.argument] = tc.value
			requests = 0

			resp := ldsConfigure(t, p, sch, false, values)
			if tc.argument == "guardrails" {
				for _, got := range []*provider.ConfigureResponse{resp, ldsConfigure(t, p, sch, true, values)} {
					if !got.Diagnostics.HasError() || got.ResourceData != nil || got.Deferred != nil {
						t.Fatalf("unknown guardrails must fail the plan: diags %v, resource data %v, deferred %v", got.Diagnostics, got.ResourceData, got.Deferred)
					}
				}
				if requests != 0 {
					t.Fatalf("made %d requests with unknown guardrails", requests)
				}
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("an unknown value must not fail the plan: %v", resp.Diagnostics)
			}
			if resp.ResourceData != nil || resp.DataSourceData != nil || resp.Deferred != nil {
				t.Fatalf("resource data = %v, data source data = %v, deferred = %v", resp.ResourceData, resp.DataSourceData, resp.Deferred)
			}
			warnings := resp.Diagnostics.Warnings()
			if len(warnings) != 1 || !strings.Contains(warnings[0].Detail(), tc.argument) {
				t.Fatalf("warnings = %v, want one naming %s", warnings, tc.argument)
			}
			for _, secret := range []string{"key_configured", "from_environment"} {
				if strings.Contains(warnings[0].Detail(), secret) {
					t.Fatalf("the warning shows a credential: %s", warnings[0].Detail())
				}
			}

			deferred := ldsConfigure(t, p, sch, true, values)
			if len(deferred.Diagnostics) != 0 || deferred.ResourceData != nil || deferred.DataSourceData != nil {
				t.Fatalf("deferral diagnostics = %v, resource data = %v", deferred.Diagnostics, deferred.ResourceData)
			}
			if deferred.Deferred == nil || deferred.Deferred.Reason != provider.DeferredReasonProviderConfigUnknown {
				t.Fatalf("deferred = %#v", deferred.Deferred)
			}
			if requests != 0 {
				t.Fatalf("%d requests were sent with an incomplete configuration", requests)
			}
		})
	}
}

func TestUnknownProviderArgumentsNamesEachOne(t *testing.T) {
	ctx := context.Background()
	_, sch := ldsProviderSchema(t)
	typ := sch.Type().TerraformType(ctx)
	unknownString := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)

	if got := unknownProviderArguments(ldsObject(t, typ, map[string]tftypes.Value{"token": ldsString("t")})); got != nil {
		t.Fatalf("known configuration: %v", got)
	}
	if got := unknownProviderArguments(tftypes.NewValue(typ, nil)); got != nil {
		t.Fatalf("null configuration: %v", got)
	}
	got := unknownProviderArguments(ldsObject(t, typ, map[string]tftypes.Value{"token": unknownString, "api_key": unknownString, "endpoint": ldsString("e")}))
	if !slices.Equal(got, []string{"api_key", "token"}) {
		t.Fatalf("got %v, want the unknown arguments in a stable order", got)
	}
	if got := unknownProviderArguments(tftypes.NewValue(typ, tftypes.UnknownValue)); len(got) != 1 {
		t.Fatalf("wholly unknown configuration: %v", got)
	}
}

func TestConfigureCredentialPrecedence(t *testing.T) {
	p, sch := ldsProviderSchema(t)

	t.Run("argument beats environment variable", func(t *testing.T) {
		ldsClearEnv(t)
		t.Setenv(envAPIKey, "env_api")
		t.Setenv(envPoolAPIKey, "env_pool")
		t.Setenv(envTeamAPIKey, "env_team")
		t.Setenv(envOrgAPIKey, "env_org")
		t.Setenv(envOrgID, "org_env")
		client := ldsConfiguredClient(t, p, sch, map[string]tftypes.Value{
			"api_key":              ldsString(" cfg_api "),
			"pool_api_key":         ldsString("cfg_pool"),
			"team_api_key":         ldsString("cfg_team"),
			"organization_api_key": ldsString("cfg_org"),
			"organization_id":      ldsString("org_cfg"),
		})
		got := []string{client.apiKey, client.poolAPIKey, client.teamAPIKey, client.orgAPIKey, client.organizationID}
		if !slices.Equal(got, []string{"cfg_api", "cfg_pool", "cfg_team", "cfg_org", "org_cfg"}) {
			t.Fatalf("resolved = %v", got)
		}
		if client.public == nil || client.pools == nil || client.pools == client.public {
			t.Fatalf("a distinct pool key needs its own client: public=%p pools=%p", client.public, client.pools)
		}
		if client.authHeader != "" {
			t.Fatalf("no token was configured, auth header = %q", client.authHeader)
		}
	})

	t.Run("environment variable fills an omitted argument", func(t *testing.T) {
		ldsClearEnv(t)
		t.Setenv(envAPIKey, " env_api ")
		t.Setenv(envTeamAPIKey, "env_team")
		t.Setenv(envOrgAPIKey, "env_org")
		t.Setenv(envOrgID, "org_env")
		client := ldsConfiguredClient(t, p, sch, nil)
		got := []string{client.apiKey, client.poolAPIKey, client.teamAPIKey, client.orgAPIKey, client.organizationID}
		if !slices.Equal(got, []string{"env_api", "env_api", "env_team", "env_org", "org_env"}) {
			t.Fatalf("resolved = %v", got)
		}
		if client.public == nil || client.pools != client.public {
			t.Fatalf("pool_api_key defaults to api_key and shares its client: public=%p pools=%p", client.public, client.pools)
		}
	})

	t.Run("empty argument is unset and does not read the environment", func(t *testing.T) {
		ldsClearEnv(t)
		t.Setenv(envAPIKey, "env_api")
		client := ldsConfiguredClient(t, p, sch, map[string]tftypes.Value{
			"api_key":      ldsString(""),
			"team_api_key": ldsString("cfg_team"),
		})
		if client.apiKey != "" || client.public != nil || client.pools != nil {
			t.Fatalf("apiKey = %q public = %v pools = %v", client.apiKey, client.public, client.pools)
		}
		if _, err := requirePublic(client); err == nil || !strings.Contains(err.Error(), "api_key") || !strings.Contains(err.Error(), envAPIKey) {
			t.Fatalf("requirePublic error = %v", err)
		}
		if _, err := requirePools(client); err == nil || !strings.Contains(err.Error(), "pool_api_key") || !strings.Contains(err.Error(), envPoolAPIKey) {
			t.Fatalf("requirePools error = %v", err)
		}
	})

	t.Run("pool key alone", func(t *testing.T) {
		ldsClearEnv(t)
		client := ldsConfiguredClient(t, p, sch, map[string]tftypes.Value{"pool_api_key": ldsString("cfg_pool")})
		if client.public != nil || client.pools == nil || client.apiKey != "" {
			t.Fatalf("public = %v pools = %v apiKey = %q", client.public, client.pools, client.apiKey)
		}
	})

	t.Run("no credential", func(t *testing.T) {
		ldsClearEnv(t)
		resp := ldsConfigure(t, p, sch, false, map[string]tftypes.Value{"organization_id": ldsString("org_1"), "endpoint": ldsString("https://example.invalid")})
		if !resp.Diagnostics.HasError() || resp.ResourceData != nil {
			t.Fatalf("diagnostics = %v", resp.Diagnostics)
		}
		detail := resp.Diagnostics.Errors()[0].Detail()
		for _, want := range []string{"token", envToken, "api_key", "team_api_key", "organization_api_key", "pool_api_key"} {
			if !strings.Contains(detail, want) {
				t.Errorf("detail %q does not mention %s", detail, want)
			}
		}
	})
}

func TestConfigureCapturesRawTokenBeforeExchange(t *testing.T) {
	ldsClearEnv(t)
	p, sch := ldsProviderSchema(t)
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"accessToken":"session-token"}`)
	}))
	defer server.Close()

	client := ldsConfiguredClient(t, p, sch, map[string]tftypes.Value{
		"token":    ldsString(" key_raw "),
		"endpoint": ldsString(server.URL + "/"),
	})
	if !slices.Equal(seen, []string{"POST /auth/exchange_user_api_key Bearer key_raw"}) {
		t.Fatalf("exchange requests = %v", seen)
	}
	if client.authHeader != "Bearer session-token" {
		t.Fatalf("auth header = %q", client.authHeader)
	}
	if client.apiKey != "key_raw" || client.poolAPIKey != "key_raw" || client.public == nil || client.pools != client.public {
		t.Fatalf("the raw key must become api_key and pool_api_key: apiKey=%q pool=%q public=%v", client.apiKey, client.poolAPIKey, client.public)
	}

	seen = nil
	session := ldsConfiguredClient(t, p, sch, map[string]tftypes.Value{
		"token":    ldsString("session-token"),
		"endpoint": ldsString(server.URL),
	})
	if len(seen) != 0 || session.apiKey != "" || session.public != nil || session.pools != nil {
		t.Fatalf("a session token is not an API key: requests=%v apiKey=%q public=%v", seen, session.apiKey, session.public)
	}

	explicit := ldsConfiguredClient(t, p, sch, map[string]tftypes.Value{
		"token":    ldsString("key_raw"),
		"api_key":  ldsString("key_service_account"),
		"endpoint": ldsString(server.URL),
	})
	if explicit.apiKey != "key_service_account" || explicit.poolAPIKey != "key_service_account" {
		t.Fatalf("an explicit api_key must win over the raw token: apiKey=%q pool=%q", explicit.apiKey, explicit.poolAPIKey)
	}
}

func TestProviderSchemaCredentials(t *testing.T) {
	_, sch := ldsProviderSchema(t)
	envByArgument := map[string]string{
		"token":                envToken,
		"endpoint":             envEndpoint,
		"api_key":              envAPIKey,
		"team_api_key":         envTeamAPIKey,
		"organization_api_key": envOrgAPIKey,
		"organization_id":      envOrgID,
		"pool_api_key":         envPoolAPIKey,
	}
	if len(sch.Attributes) != len(envByArgument) {
		t.Fatalf("provider has %d arguments, this test knows %d; add the new one here", len(sch.Attributes), len(envByArgument))
	}
	for name, attribute := range sch.Attributes {
		stringAttribute, ok := attribute.(pschema.StringAttribute)
		if !ok {
			t.Fatalf("%s is %T", name, attribute)
		}
		if !stringAttribute.Optional || stringAttribute.Required {
			t.Errorf("%s must be optional: every argument has an environment variable", name)
		}
		credential := strings.HasSuffix(name, "_key") || name == "token"
		if stringAttribute.Sensitive != credential {
			t.Errorf("%s sensitive = %t, want %t", name, stringAttribute.Sensitive, credential)
		}
		if !strings.Contains(stringAttribute.Description, envByArgument[name]) {
			t.Errorf("%s description does not name %s: %s", name, envByArgument[name], stringAttribute.Description)
		}
	}
	for _, want := range []string{"takes precedence over its environment variable", "known only after apply"} {
		if !strings.Contains(sch.Description, want) {
			t.Errorf("provider description does not say %q", want)
		}
	}
}

func ldsForkDataSources() map[string]func() datasource.DataSource {
	return map[string]func() datasource.DataSource{
		"cursor_api_key_info":             NewAPIKeyInfoDataSource,
		"cursor_team_members":             NewTeamMembersDataSource,
		"cursor_environment":              NewEnvironmentDataSource,
		"cursor_environments":             NewEnvironmentsDataSource,
		"cursor_environment_active_build": NewEnvironmentActiveBuildDataSource,
		"cursor_models":                   NewModelsDataSource,
		"cursor_private_worker_pools":     NewPrivateWorkerPoolsDataSource,
		"cursor_private_workers_summary":  NewPrivateWorkersSummaryDataSource,
	}
}

func ldsTypeName(register any) string {
	name := runtime.FuncForPC(reflect.ValueOf(register).Pointer()).Name()
	return name[strings.LastIndex(name, ".")+1:]
}

// Every constructor in the package must be registered, and every registered type
// must have a generated docs page and an example for the docs generator to embed.
func TestEveryTypeIsRegisteredDocumentedAndHasAnExample(t *testing.T) {
	ctx := context.Background()
	p := &cursorProvider{version: "test"}
	registered := map[string]bool{}
	check := func(kind, typeName, exampleFile string) {
		short := strings.TrimPrefix(typeName, "cursor_")
		for _, file := range []string{
			filepath.Join("..", "..", "docs", kind, short+".md"),
			filepath.Join("..", "..", "examples", kind, typeName, exampleFile),
		} {
			if info, err := os.Stat(file); err != nil || info.Size() == 0 {
				t.Errorf("%s has no %s", typeName, file)
			}
		}
	}

	dataSourceNames := map[string]bool{}
	for _, register := range p.DataSources(ctx) {
		registered[ldsTypeName(register)] = true
		resp := &datasource.MetadataResponse{}
		register().Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "cursor"}, resp)
		if dataSourceNames[resp.TypeName] {
			t.Errorf("data source %s is registered twice", resp.TypeName)
		}
		dataSourceNames[resp.TypeName] = true
		check("data-sources", resp.TypeName, "data-source.tf")
	}
	for name := range ldsForkDataSources() {
		if !dataSourceNames[name] {
			t.Errorf("data source %s is not registered", name)
		}
	}
	resourceNames := map[string]bool{}
	for _, register := range p.Resources(ctx) {
		registered[ldsTypeName(register)] = true
		resp := &resource.MetadataResponse{}
		register().Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "cursor"}, resp)
		if resourceNames[resp.TypeName] {
			t.Errorf("resource %s is registered twice", resp.TypeName)
		}
		resourceNames[resp.TypeName] = true
		check("resources", resp.TypeName, "resource.tf")
	}
	for _, file := range []string{filepath.Join("..", "..", "docs", "index.md"), filepath.Join("..", "..", "examples", "provider", "provider.tf")} {
		if info, err := os.Stat(file); err != nil || info.Size() == 0 {
			t.Errorf("provider has no %s", file)
		}
	}

	constructor := regexp.MustCompile(`^New\w+(Resource|DataSource)$`)
	packages, err := parser.ParseDir(token.NewFileSet(), ".", func(info os.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !constructor.MatchString(fn.Name.Name) {
					continue
				}
				found++
				if !registered[fn.Name.Name] {
					t.Errorf("%s is defined but not registered in provider.go", fn.Name.Name)
				}
			}
		}
	}
	if found != len(registered) {
		t.Errorf("found %d constructors in the package and %d registrations", found, len(registered))
	}
}

func ldsDataSchema(t *testing.T, ds datasource.DataSource) dschema.Schema {
	t.Helper()
	resp := &datasource.SchemaResponse{}
	ds.Schema(context.Background(), datasource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.Schema
}

// ldsReadDataSource runs Read with a configuration that sets only the given attributes.
func ldsReadDataSource(t *testing.T, ctx context.Context, ds datasource.DataSource, config map[string]tftypes.Value) *datasource.ReadResponse {
	t.Helper()
	sch := ldsDataSchema(t, ds)
	typ := sch.Type().TerraformType(ctx)
	resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(typ, nil)}}
	ds.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Schema: sch, Raw: ldsObject(t, typ, config)}}, resp)
	return resp
}

func ldsMustRead(t *testing.T, ctx context.Context, ds datasource.DataSource, config map[string]tftypes.Value, target any) {
	t.Helper()
	resp := ldsReadDataSource(t, ctx, ds, config)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if diags := resp.State.Get(ctx, target); diags.HasError() {
		t.Fatal(diags)
	}
}

func ldsDiagText(diags diag.Diagnostics) string {
	var b strings.Builder
	for _, d := range diags {
		fmt.Fprintf(&b, "%s: %s: %s\n", d.Severity(), d.Summary(), d.Detail())
	}
	return b.String()
}

func TestForkDataSourcesFailClearlyWithoutAConfiguredProvider(t *testing.T) {
	ctx := context.Background()
	want := map[string]string{
		"cursor_api_key_info":             "api_key",
		"cursor_team_members":             "team_api_key",
		"cursor_environment":              "api_key",
		"cursor_environments":             "api_key",
		"cursor_environment_active_build": "api_key",
		"cursor_models":                   "api_key",
		"cursor_private_worker_pools":     "pool_api_key",
		"cursor_private_workers_summary":  "pool_api_key",
	}
	config := map[string]map[string]tftypes.Value{
		"cursor_environment":              {"environment_id": ldsString("env-1")},
		"cursor_environment_active_build": {"environment_id": ldsString("env-1")},
	}
	for name, construct := range ldsForkDataSources() {
		// nil is what a data source receives while the provider is deferred; the empty
		// client is a provider configured with a credential this data source does not use.
		for label, data := range map[string]any{"deferred provider": nil, "other credential only": &apiClient{}} {
			t.Run(name+"/"+label, func(t *testing.T) {
				ds := construct()
				configure := &datasource.ConfigureResponse{}
				ds.(datasource.DataSourceWithConfigure).Configure(ctx, datasource.ConfigureRequest{ProviderData: data}, configure)
				if configure.Diagnostics.HasError() {
					t.Fatal(configure.Diagnostics)
				}
				resp := ldsReadDataSource(t, ctx, ds, config[name])
				if !resp.Diagnostics.HasError() {
					t.Fatal("a read without a client must fail")
				}
				if text := ldsDiagText(resp.Diagnostics); !strings.Contains(text, want[name]+" is required") {
					t.Fatalf("diagnostics do not name %s: %s", want[name], text)
				}
			})
		}
		t.Run(name+"/wrong provider data", func(t *testing.T) {
			configure := &datasource.ConfigureResponse{}
			construct().(datasource.DataSourceWithConfigure).Configure(ctx, datasource.ConfigureRequest{ProviderData: "not a client"}, configure)
			if !configure.Diagnostics.HasError() {
				t.Fatal("an unexpected provider data type must be reported")
			}
		})
	}
}

// Calls every operation of every registered resource and data source the way
// Terraform does while the provider is unconfigured. None may dereference the
// missing client.
func TestNoTypePanicsWithoutAConfiguredProvider(t *testing.T) {
	ctx := context.Background()
	p := &cursorProvider{version: "test"}
	// Updates listed here only compare plan and state; they never call the API.
	localUpdate := map[string]bool{"cursor_origin_ssh_certificate_authority": true}
	guard := func(t *testing.T, operation string, needsClient bool, fn func() diag.Diagnostics) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("%s panicked without a configured provider: %v", operation, r)
			}
		}()
		if diags := fn(); needsClient && !diags.HasError() {
			t.Errorf("%s reported no error without a configured provider", operation)
		}
	}
	for _, register := range p.DataSources(ctx) {
		ds := register()
		meta := &datasource.MetadataResponse{}
		ds.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "cursor"}, meta)
		t.Run(meta.TypeName, func(t *testing.T) {
			if configurable, ok := ds.(datasource.DataSourceWithConfigure); ok {
				configure := &datasource.ConfigureResponse{}
				configurable.Configure(ctx, datasource.ConfigureRequest{}, configure)
				if configure.Diagnostics.HasError() {
					t.Fatal(configure.Diagnostics)
				}
			}
			guard(t, "Read", true, func() diag.Diagnostics { return ldsReadDataSource(t, ctx, ds, nil).Diagnostics })
		})
	}
	for _, register := range p.Resources(ctx) {
		res := register()
		meta := &resource.MetadataResponse{}
		res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "cursor"}, meta)
		t.Run(meta.TypeName, func(t *testing.T) {
			if configurable, ok := res.(resource.ResourceWithConfigure); ok {
				configure := &resource.ConfigureResponse{}
				configurable.Configure(ctx, resource.ConfigureRequest{}, configure)
				if configure.Diagnostics.HasError() {
					t.Fatal(configure.Diagnostics)
				}
			}
			schemaResp := &resource.SchemaResponse{}
			res.Schema(ctx, resource.SchemaRequest{}, schemaResp)
			sch := schemaResp.Schema
			raw := ldsObject(t, sch.Type().TerraformType(ctx), nil)
			config := tfsdk.Config{Schema: sch, Raw: raw}
			plan := tfsdk.Plan{Schema: sch, Raw: raw}
			state := tfsdk.State{Schema: sch, Raw: raw}
			guard(t, "Create", true, func() diag.Diagnostics {
				resp := &resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
				res.Create(ctx, resource.CreateRequest{Config: config, Plan: plan}, resp)
				return resp.Diagnostics
			})
			guard(t, "Read", true, func() diag.Diagnostics {
				resp := &resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
				res.Read(ctx, resource.ReadRequest{State: state}, resp)
				return resp.Diagnostics
			})
			guard(t, "Update", !localUpdate[meta.TypeName], func() diag.Diagnostics {
				resp := &resource.UpdateResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
				res.Update(ctx, resource.UpdateRequest{Config: config, Plan: plan, State: state}, resp)
				return resp.Diagnostics
			})
			guard(t, "Delete", true, func() diag.Diagnostics {
				resp := &resource.DeleteResponse{State: tfsdk.State{Schema: sch, Raw: raw}}
				res.Delete(ctx, resource.DeleteRequest{State: state}, resp)
				return resp.Diagnostics
			})
			if planner, ok := res.(resource.ResourceWithModifyPlan); ok {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("ModifyPlan panicked without a configured provider: %v", r)
						}
					}()
					resp := &resource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: sch, Raw: raw}}
					planner.ModifyPlan(ctx, resource.ModifyPlanRequest{Config: config, Plan: plan, State: state}, resp)
				}()
			}
		})
	}
}

type ldsServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
}

func (s *ldsServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// newLdsServer serves each path with a fixed status and body. A path may list
// several responses; they are returned in order and the last one repeats.
func newLdsServer(t *testing.T, routes map[string][]stubResponse) *ldsServer {
	t.Helper()
	s := &ldsServer{}
	calls := map[string]int{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
		responses, ok := routes[r.URL.Path]
		var next stubResponse
		if ok {
			next = responses[min(calls[r.URL.Path], len(responses)-1)]
			calls[r.URL.Path]++
		}
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"no such route"}}`)
			return
		}
		if next.status != 0 {
			w.WriteHeader(next.status)
		}
		_, _ = io.WriteString(w, next.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func ldsOK(body string) []stubResponse { return []stubResponse{{body: body}} }

func ldsListIsEmptyNotNull(t *testing.T, ctx context.Context, state tfsdk.State, attribute string) {
	t.Helper()
	var list types.List
	if diags := state.GetAttribute(ctx, path.Root(attribute), &list); diags.HasError() {
		t.Fatal(diags)
	}
	if list.IsNull() || list.IsUnknown() || len(list.Elements()) != 0 {
		t.Fatalf("%s = %s, want an empty list that for expressions and length() accept", attribute, list)
	}
}

func TestListDataSourcesReturnEmptyListsNotNull(t *testing.T) {
	ctx := context.Background()
	server := newLdsServer(t, map[string][]stubResponse{
		"/v1/environments":          ldsOK(`{"items":[]}`),
		"/v1/models":                ldsOK(`{"items":null}`),
		"/v0/private-workers/pools": ldsOK(`{}`),
		"/teams/members":            ldsOK(`{"teamMembers":[]}`),
	})
	client := publicClient(server.URL, "key_test")
	client.adminBase, client.teamAPIKey = server.URL, "team"
	cases := map[string]struct {
		ds        datasource.DataSource
		attribute string
	}{
		"environments": {&environmentsDataSource{client: client}, "environments"},
		"models":       {&modelsDataSource{client: client}, "models"},
		"pools":        {&privateWorkerPoolsDataSource{client: client}, "pools"},
		"team members": {&teamMembersDataSource{client: client}, "members"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp := ldsReadDataSource(t, ctx, tc.ds, nil)
			if resp.Diagnostics.HasError() {
				t.Fatal(resp.Diagnostics)
			}
			ldsListIsEmptyNotNull(t, ctx, resp.State, tc.attribute)
		})
	}
}

func TestNestedListsAreEmptyNotNull(t *testing.T) {
	ctx := context.Background()
	server := newLdsServer(t, map[string][]stubResponse{
		"/v1/environments":       ldsOK(`{"items":[{"id":"env-1","owner":"team","createdAt":"c","updatedAt":"u"}]}`),
		"/v1/environments/env-1": ldsOK(`{"id":"env-1","owner":"team","repos":null,"createdAt":"c","updatedAt":"u"}`),
		"/v1/models":             ldsOK(`{"items":[{"id":"bare","displayName":"Bare"},{"id":"full","displayName":"Full","parameters":[{"id":"p"}],"variants":[{"displayName":"V","params":[]}]}]}`),
	})
	client := publicClient(server.URL, "key_test")

	var envs environmentsModel
	ldsMustRead(t, ctx, &environmentsDataSource{client: client}, nil, &envs)
	if len(envs.Environments) != 1 || envs.Environments[0].Repos == nil || !envs.Environments[0].Name.IsNull() {
		t.Fatalf("environments = %#v", envs.Environments)
	}

	var env environmentDataModel
	ldsMustRead(t, ctx, &environmentDataSource{client: client}, map[string]tftypes.Value{"environment_id": ldsString("env-1")}, &env)
	if env.Repos == nil || len(env.Repos) != 0 {
		t.Fatalf("repos = %#v", env.Repos)
	}

	var models modelsModel
	ldsMustRead(t, ctx, &modelsDataSource{client: client}, nil, &models)
	bare, full := models.Models[0], models.Models[1]
	if bare.Aliases.IsNull() || bare.Parameters == nil || bare.Variants == nil || !bare.Description.IsNull() {
		t.Fatalf("bare model = %#v", bare)
	}
	if len(full.Parameters) != 1 || full.Parameters[0].Values == nil || !full.Parameters[0].DisplayName.IsNull() {
		t.Fatalf("parameters = %#v", full.Parameters)
	}
	if len(full.Variants) != 1 || full.Variants[0].Params == nil || !full.Variants[0].Description.IsNull() || full.Variants[0].IsDefault.ValueBool() {
		t.Fatalf("variants = %#v", full.Variants)
	}
}

func TestListDataSourcesReturnAStableOrder(t *testing.T) {
	ctx := context.Background()
	env := func(id string) string {
		return `{"id":"` + id + `","owner":"team","repos":[],"createdAt":"c","updatedAt":"u"}`
	}
	server := newLdsServer(t, map[string][]stubResponse{
		// The API lists the most recently updated environment first, and b is
		// repeated on the second page because it moved during the walk.
		"/v1/environments": {
			{body: `{"items":[` + env("c") + `,` + env("b") + `],"nextCursor":"p2"}`},
			{body: `{"items":[` + env("b") + `,` + env("a") + `]}`},
		},
		"/v1/models":                ldsOK(`{"items":[{"id":"zeta","displayName":"Z","aliases":["z-latest","z"]},{"id":"alpha","displayName":"A"}]}`),
		"/v0/private-workers/pools": ldsOK(`{"pools":[{"scope":"user","poolName":"cpu"},{"scope":"team","poolName":"gpu","repoOwner":"acme","repoName":"api"},{"scope":"team","poolName":"gpu"}]}`),
		"/teams/members":            ldsOK(`{"teamMembers":[{"id":"user_3","email":"zoe@acme.com","role":"member"},{"id":"user_2","email":"Amy@acme.com","role":"owner"},{"id":"user_1","email":"amy@acme.com","role":"member","isRemoved":true}]}`),
	})
	client := publicClient(server.URL, "key_test")
	client.adminBase, client.teamAPIKey = server.URL, "team"

	var envs environmentsModel
	ldsMustRead(t, ctx, &environmentsDataSource{client: client}, nil, &envs)
	var envIDs []string
	for _, item := range envs.Environments {
		envIDs = append(envIDs, item.ID.ValueString())
	}
	if !slices.Equal(envIDs, []string{"a", "b", "c"}) {
		t.Fatalf("environment ids = %v, want every page, each id once, sorted", envIDs)
	}
	if requests := server.seen(); len(requests) != 2 || !strings.Contains(requests[1], "cursor=p2") {
		t.Fatalf("requests = %v", requests)
	}

	var models modelsModel
	ldsMustRead(t, ctx, &modelsDataSource{client: client}, nil, &models)
	var aliases []string
	models.Models[1].Aliases.ElementsAs(ctx, &aliases, false)
	if models.Models[0].ID.ValueString() != "alpha" || models.Models[1].ID.ValueString() != "zeta" || !slices.Equal(aliases, []string{"z-latest", "z"}) {
		t.Fatalf("models = %#v aliases = %v", models.Models, aliases)
	}

	var pools privateWorkerPoolsModel
	ldsMustRead(t, ctx, &privateWorkerPoolsDataSource{client: client}, nil, &pools)
	var poolIDs []string
	for _, item := range pools.Pools {
		poolIDs = append(poolIDs, item.ID.ValueString())
	}
	if !slices.Equal(poolIDs, []string{"team/gpu", "team/gpu/acme/api", "user/cpu"}) {
		t.Fatalf("pool ids = %v", poolIDs)
	}

	var members teamMembersModel
	ldsMustRead(t, ctx, &teamMembersDataSource{client: client}, nil, &members)
	var memberIDs []string
	for _, item := range members.Members {
		memberIDs = append(memberIDs, item.ID.ValueString())
	}
	if !slices.Equal(memberIDs, []string{"user_1", "user_2", "user_3"}) {
		t.Fatalf("member ids = %v, want email order ignoring case, then id", memberIDs)
	}
	if !members.Members[0].IsRemoved.ValueBool() || members.Members[1].Role.ValueString() != "owner" {
		t.Fatalf("members = %#v", members.Members)
	}
}

func TestEnvironmentDataSourcePopulatesEveryAttribute(t *testing.T) {
	ctx := context.Background()
	const id = "8F14E45F-CEEA-4E6B-9C3A-1D2E3F4A5B6C"
	server := newLdsServer(t, map[string][]stubResponse{
		"/v1/environments/" + id: ldsOK(`{"id":"8f14e45f-ceea-4e6b-9c3a-1d2e3f4a5b6c","name":"web","owner":"team","repos":[{"url":"https://github.com/acme/web"},{"url":"https://github.com/acme/api"}],"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-02-01T00:00:00Z","environmentJson":"{\"install\":\"pnpm\"}","versionId":"v-1","repoFile":{"url":"https://github.com/acme/web","path":".cursor/environment.json"}}`),
		"/v1/environments":       ldsOK(`{"items":[{"id":"env-file","owner":"personal","repos":[],"createdAt":"c","updatedAt":"u","repoFile":{"url":"https://github.com/acme/web","path":".cursor/environment.json"}}]}`),
	})
	client := publicClient(server.URL, "key_test")

	var env environmentDataModel
	ldsMustRead(t, ctx, &environmentDataSource{client: client}, map[string]tftypes.Value{"environment_id": ldsString(id)}, &env)
	got := map[string]string{
		"id": env.ID.ValueString(), "environment_id": env.EnvironmentID.ValueString(), "name": env.Name.ValueString(), "owner": env.Owner.ValueString(),
		"environment_json": env.EnvironmentJSON.ValueString(), "version_id": env.VersionID.ValueString(),
		"repo_file_url": env.RepoFileURL.ValueString(), "repo_file_path": env.RepoFilePath.ValueString(),
		"created_at": env.CreatedAt.ValueString(), "updated_at": env.UpdatedAt.ValueString(),
	}
	want := map[string]string{
		// Terraform rejects a data source that returns a required argument changed
		// from the configuration, so environment_id keeps the configured spelling.
		"id": "8f14e45f-ceea-4e6b-9c3a-1d2e3f4a5b6c", "environment_id": id, "name": "web", "owner": "team",
		"environment_json": `{"install":"pnpm"}`, "version_id": "v-1",
		"repo_file_url": "https://github.com/acme/web", "repo_file_path": ".cursor/environment.json",
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-02-01T00:00:00Z",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if len(env.Repos) != 2 || env.Repos[0].URL.ValueString() != "https://github.com/acme/web" || env.Repos[1].URL.ValueString() != "https://github.com/acme/api" {
		t.Fatalf("repos = %#v, want the API order", env.Repos)
	}

	var envs environmentsModel
	ldsMustRead(t, ctx, &environmentsDataSource{client: client}, nil, &envs)
	item := envs.Environments[0]
	if item.RepoFileURL.ValueString() != "https://github.com/acme/web" || item.RepoFilePath.ValueString() != ".cursor/environment.json" || item.Owner.ValueString() != "personal" || item.CreatedAt.ValueString() != "c" || item.UpdatedAt.ValueString() != "u" {
		t.Fatalf("list item = %#v", item)
	}
}

func TestEnvironmentDataSourcesReportNotFound(t *testing.T) {
	ctx := context.Background()
	notFound := []stubResponse{{status: http.StatusNotFound, body: `{"error":{"code":"environment_not_found","message":"Environment not found"}}`}}
	server := newLdsServer(t, map[string][]stubResponse{
		"/v1/environments/gone":               notFound,
		"/v1/environments/gone/builds/active": notFound,
		"/v1/environments/down":               {{status: http.StatusInternalServerError, body: `{"error":{"code":"internal_error","message":"boom"}}`}},
		"/v1/environments/down/builds/active": {{status: http.StatusForbidden, body: `{"error":{"code":"feature_unavailable","message":"no"}}`}},
	})
	client := publicClient(server.URL, "key_test")
	sources := map[string]datasource.DataSource{
		"environment":  &environmentDataSource{client: client},
		"active build": &environmentActiveBuildDataSource{client: client},
	}
	for name, ds := range sources {
		t.Run(name, func(t *testing.T) {
			resp := ldsReadDataSource(t, ctx, ds, map[string]tftypes.Value{"environment_id": ldsString("gone")})
			errs := resp.Diagnostics.Errors()
			if len(errs) != 1 || errs[0].Summary() != "Cursor environment not found" {
				t.Fatalf("diagnostics = %s", ldsDiagText(resp.Diagnostics))
			}
			withPath, ok := errs[0].(diag.DiagnosticWithPath)
			if !ok || !withPath.Path().Equal(path.Root("environment_id")) {
				t.Fatalf("the error does not point at environment_id: %#v", errs[0])
			}
			for _, wantText := range []string{`"gone"`, "does not exist or the key cannot access it", "environment_not_found"} {
				if !strings.Contains(errs[0].Detail(), wantText) {
					t.Errorf("detail %q does not contain %q", errs[0].Detail(), wantText)
				}
			}
			if !resp.State.Raw.IsNull() {
				t.Fatal("a failed read must not write state")
			}

			other := ldsReadDataSource(t, ctx, ds, map[string]tftypes.Value{"environment_id": ldsString("down")})
			text := ldsDiagText(other.Diagnostics)
			if !other.Diagnostics.HasError() || strings.Contains(text, "not found") || !strings.Contains(text, "Failed to read") {
				t.Fatalf("another failure must not be reported as not found: %s", text)
			}
		})
	}
}

func TestActiveBuildDataSource(t *testing.T) {
	ctx := context.Background()
	server := newLdsServer(t, map[string][]stubResponse{
		"/v1/environments/image/builds/active": ldsOK(`{"type":"universal_image"}`),
		"/v1/environments/built/builds/active": ldsOK(`{"type":"build","buildId":"bld-1"}`),
		"/v1/environments/built/builds/bld-1":  ldsOK(`{"id":"bld-1","environmentId":"built","status":"IN_PROGRESS"}`),
		"/v1/environments/raced/builds/active": ldsOK(`{"type":"build","buildId":"bld-2"}`),
		"/v1/environments/raced/builds/bld-2":  {{status: http.StatusNotFound, body: `{"error":{"code":"build_not_found","message":"Build not found"}}`}},
		"/v1/environments/empty/builds/active": ldsOK(`{}`),
	})
	client := publicClient(server.URL, "key_test")
	ds := &environmentActiveBuildDataSource{client: client}

	var image environmentActiveBuildModel
	ldsMustRead(t, ctx, ds, map[string]tftypes.Value{"environment_id": ldsString("image")}, &image)
	if image.Type.ValueString() != "universal_image" || !image.BuildID.IsNull() || !image.Status.IsNull() || image.ID.ValueString() != "image" {
		t.Fatalf("universal image = %#v", image)
	}
	if requests := server.seen(); len(requests) != 1 {
		t.Fatalf("universal_image has no build to read: %v", requests)
	}

	var built environmentActiveBuildModel
	ldsMustRead(t, ctx, ds, map[string]tftypes.Value{"environment_id": ldsString("built")}, &built)
	if built.Type.ValueString() != "build" || built.BuildID.ValueString() != "bld-1" || built.Status.ValueString() != "IN_PROGRESS" {
		t.Fatalf("build = %#v", built)
	}

	raced := ldsReadDataSource(t, ctx, ds, map[string]tftypes.Value{"environment_id": ldsString("raced")})
	text := ldsDiagText(raced.Diagnostics)
	if !raced.Diagnostics.HasError() || !strings.Contains(text, "Failed to read environment build") || !strings.Contains(text, "build_not_found") || strings.Contains(text, "Cursor environment not found") {
		t.Fatalf("a missing build is not a missing environment: %s", text)
	}

	empty := ldsReadDataSource(t, ctx, ds, map[string]tftypes.Value{"environment_id": ldsString("empty")})
	if !empty.Diagnostics.HasError() || !strings.Contains(ldsDiagText(empty.Diagnostics), "did not include a type") {
		t.Fatalf("a response without a type must fail: %s", ldsDiagText(empty.Diagnostics))
	}
}

func TestEnvironmentDataSourceRejectsAResponseWithoutAnID(t *testing.T) {
	ctx := context.Background()
	server := newLdsServer(t, map[string][]stubResponse{
		"/v1/environments/env-1": ldsOK(`{"items":[{"id":"other","owner":"team"}]}`),
	})
	resp := ldsReadDataSource(t, ctx, &environmentDataSource{client: publicClient(server.URL, "key_test")}, map[string]tftypes.Value{"environment_id": ldsString("env-1")})
	if !resp.Diagnostics.HasError() || !strings.Contains(ldsDiagText(resp.Diagnostics), "did not include an id") || !resp.State.Raw.IsNull() {
		t.Fatalf("diagnostics = %s", ldsDiagText(resp.Diagnostics))
	}
}

func ldsValidateString(ctx context.Context, validators []validator.String, value types.String) diag.Diagnostics {
	var diags diag.Diagnostics
	for _, v := range validators {
		resp := &validator.StringResponse{}
		v.ValidateString(ctx, validator.StringRequest{Path: path.Root("value"), ConfigValue: value}, resp)
		diags.Append(resp.Diagnostics...)
	}
	return diags
}

func TestDataSourceArgumentValidation(t *testing.T) {
	ctx := context.Background()
	for name, ds := range map[string]datasource.DataSource{"environment": &environmentDataSource{}, "active build": &environmentActiveBuildDataSource{}} {
		attribute, ok := ldsDataSchema(t, ds).Attributes["environment_id"].(dschema.StringAttribute)
		if !ok || !attribute.Required || attribute.Optional || attribute.Computed || len(attribute.Validators) == 0 {
			t.Fatalf("%s environment_id = %#v, want a required validated string", name, attribute)
		}
		for _, bad := range []string{"", " ", "8f14e45f ceea", "\tenv", "env\n"} {
			if !ldsValidateString(ctx, attribute.Validators, types.StringValue(bad)).HasError() {
				t.Errorf("%s accepted environment_id %q", name, bad)
			}
		}
		for _, good := range []types.String{types.StringValue("8f14e45f-ceea-4e6b-9c3a-1d2e3f4a5b6c"), types.StringValue("env-1"), types.StringUnknown(), types.StringNull()} {
			if diags := ldsValidateString(ctx, attribute.Validators, good); diags.HasError() {
				t.Errorf("%s rejected environment_id %s: %v", name, good, diags)
			}
		}
	}

	scope, ok := ldsDataSchema(t, &privateWorkerPoolsDataSource{}).Attributes["scope"].(dschema.StringAttribute)
	if !ok || !scope.Optional || scope.Computed || scope.Required {
		t.Fatalf("scope = %#v", scope)
	}
	for _, bad := range []string{"", "everything", "Team", "team_pool "} {
		if !ldsValidateString(ctx, scope.Validators, types.StringValue(bad)).HasError() {
			t.Errorf("accepted scope %q", bad)
		}
	}
	for _, good := range []types.String{types.StringValue("team_pool"), types.StringValue("personal"), types.StringValue("all"), types.StringValue("team"), types.StringValue("user"), types.StringUnknown(), types.StringNull()} {
		if diags := ldsValidateString(ctx, scope.Validators, good); diags.HasError() {
			t.Errorf("rejected scope %s: %v", good, diags)
		}
	}
}

func TestPrivateWorkerPoolsScopeAndIDs(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		scope     tftypes.Value
		wantQuery string
		body      string
		wantIDs   []string
		wantScope []string
	}{
		{
			name: "resource scope team is sent as team_pool", scope: ldsString("team"), wantQuery: "team_pool",
			body:    `{"pools":[{"scope":"team_pool","poolName":"gpu","repoOwner":"acme","repoName":"api","repoUrl":"https://github.com/acme/api"},{"poolName":"a/b"}]}`,
			wantIDs: []string{"team/a%2Fb", "team/gpu/acme/api"}, wantScope: []string{"team", "team"},
		},
		{
			name: "resource scope user is sent as personal", scope: ldsString("user"), wantQuery: "personal",
			body:    `{"pools":[{"scope":"personal","poolName":"cpu"}]}`,
			wantIDs: []string{"user/cpu"}, wantScope: []string{"user"},
		},
		{
			name: "list scope is sent unchanged", scope: ldsString("personal"), wantQuery: "personal",
			body:    `{"pools":[{"scope":"user","poolName":"cpu"}]}`,
			wantIDs: []string{"user/cpu"}, wantScope: []string{"user"},
		},
		{
			name: "omitted scope lists everything", scope: tftypes.NewValue(tftypes.String, nil), wantQuery: "all",
			body:    `{"pools":[{"scope":"personal","poolName":"cpu"},{"scope":"team_pool","poolName":"gpu"},{"poolName":"unscoped"}]}`,
			wantIDs: []string{"/unscoped", "team/gpu", "user/cpu"}, wantScope: []string{"", "team", "user"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newLdsServer(t, map[string][]stubResponse{"/v0/private-workers/pools": ldsOK(tc.body)})
			var got privateWorkerPoolsModel
			ldsMustRead(t, ctx, &privateWorkerPoolsDataSource{client: publicClient(server.URL, "key_pool")}, map[string]tftypes.Value{"scope": tc.scope}, &got)
			if requests := server.seen(); len(requests) != 1 || requests[0] != "GET /v0/private-workers/pools?scope="+tc.wantQuery {
				t.Fatalf("requests = %v", requests)
			}
			configured := types.StringNull()
			if !tc.scope.IsNull() {
				var value string
				if err := tc.scope.As(&value); err != nil {
					t.Fatal(err)
				}
				configured = types.StringValue(value)
			}
			if !got.Scope.Equal(configured) || got.ID.ValueString() != tc.wantQuery {
				t.Fatalf("scope = %s (configured %s), id = %s", got.Scope, configured, got.ID)
			}
			var ids, scopes []string
			for _, pool := range got.Pools {
				ids = append(ids, pool.ID.ValueString())
				scopes = append(scopes, pool.Scope.ValueString())
				if pool.Scope.ValueString() == "" && !pool.Scope.IsNull() {
					t.Errorf("pool %s has an empty scope instead of null", pool.ID)
				}
			}
			if !slices.Equal(ids, tc.wantIDs) || !slices.Equal(scopes, tc.wantScope) {
				t.Fatalf("ids = %v scopes = %v", ids, scopes)
			}
			for _, id := range ids {
				if strings.HasPrefix(id, "/") {
					continue
				}
				if _, _, _, _, err := parsePoolID(id); err != nil {
					t.Errorf("listed id %q is not an import id for cursor_private_worker_pool: %v", id, err)
				}
			}
		})
	}
}

func TestPrivateWorkerPoolRepoAttributes(t *testing.T) {
	ctx := context.Background()
	server := newLdsServer(t, map[string][]stubResponse{
		"/v0/private-workers/pools":   ldsOK(`{"pools":[{"scope":"team","poolName":"any"},{"scope":"team","poolName":"repo","repoOwner":"acme","repoName":"api","repoUrl":"https://github.com/acme/api","connectedWorkerCount":3,"inUseWorkerCount":2,"workerReadyTimeoutSeconds":120,"isStale":true}]}`),
		"/v0/private-workers/summary": ldsOK(`{"teamSummary":{"totalConnected":3,"inUse":2}}`),
	})
	client := publicClient(server.URL, "key_pool")
	var got privateWorkerPoolsModel
	ldsMustRead(t, ctx, &privateWorkerPoolsDataSource{client: client}, nil, &got)
	anyRepo, repo := got.Pools[0], got.Pools[1]
	if !anyRepo.RepoOwner.IsNull() || !anyRepo.RepoName.IsNull() || !anyRepo.RepoURL.IsNull() || anyRepo.ConnectedWorkerCount.ValueInt64() != 0 || anyRepo.IsStale.ValueBool() {
		t.Fatalf("any-repo pool = %#v", anyRepo)
	}
	if repo.RepoOwner.ValueString() != "acme" || repo.RepoName.ValueString() != "api" || repo.RepoURL.ValueString() != "https://github.com/acme/api" || repo.ConnectedWorkerCount.ValueInt64() != 3 || repo.InUseWorkerCount.ValueInt64() != 2 || repo.WorkerReadyTimeoutSeconds.ValueInt64() != 120 || !repo.IsStale.ValueBool() {
		t.Fatalf("repo pool = %#v", repo)
	}

	var summary privateWorkersSummaryModel
	ldsMustRead(t, ctx, &privateWorkersSummaryDataSource{client: client}, nil, &summary)
	if summary.TeamTotalConnected.ValueInt64() != 3 || summary.TeamInUse.ValueInt64() != 2 || !summary.UserTotalConnected.IsNull() || !summary.UserInUse.IsNull() {
		t.Fatalf("summary = %#v, want a missing user summary to be null and not zero", summary)
	}
}

func TestAPIKeyInfoUserKey(t *testing.T) {
	ctx := context.Background()
	server := newLdsServer(t, map[string][]stubResponse{
		"/v1/me": ldsOK(`{"apiKeyName":"laptop","createdAt":"2026-01-01T00:00:00Z","userId":42,"userEmail":"dev@acme.com","userFirstName":"Alex"}`),
	})
	var info apiKeyInfoModel
	ldsMustRead(t, ctx, &apiKeyInfoDataSource{client: publicClient(server.URL, "key_test")}, nil, &info)
	if info.ID.ValueString() != "laptop" || info.UserID.ValueString() != "42" || info.UserEmail.ValueString() != "dev@acme.com" || info.UserFirstName.ValueString() != "Alex" || !info.UserLastName.IsNull() {
		t.Fatalf("info = %#v", info)
	}
}

func TestForkDataSourceSchemas(t *testing.T) {
	for name, construct := range ldsForkDataSources() {
		sch := ldsDataSchema(t, construct())
		if sch.Description == "" {
			t.Errorf("%s has no description", name)
		}
		id, ok := sch.Attributes["id"].(dschema.StringAttribute)
		if !ok || !id.Computed {
			t.Errorf("%s has no computed id", name)
		}
		var walk func(prefix string, attributes map[string]dschema.Attribute)
		walk = func(prefix string, attributes map[string]dschema.Attribute) {
			for attrName, attribute := range attributes {
				full := prefix + attrName
				if attribute.GetDescription() == "" {
					t.Errorf("%s.%s has no description", name, full)
				}
				if attribute.IsSensitive() {
					t.Errorf("%s.%s is sensitive; these data sources return no credential or secret value", name, full)
				}
				if attribute.IsRequired() == attribute.IsComputed() && !attribute.IsOptional() {
					t.Errorf("%s.%s must be exactly one of required, optional, or computed", name, full)
				}
				if nested, ok := attribute.(dschema.ListNestedAttribute); ok {
					walk(full+".", nested.NestedObject.Attributes)
				}
			}
		}
		walk("", sch.Attributes)
	}
}

// A retried secret write must replay the whole body, and neither the retry log
// nor the final error may contain the secret value or the API key.
func TestSecretWriteRetriesWithoutLoggingTheValue(t *testing.T) {
	const secret = "super-secret-value"
	const apiKey = "key_do_not_log"
	server, bodies := serveSequence(t,
		stubResponse{status: http.StatusServiceUnavailable, body: `{"error":{"code":"upstream_error","message":"echo super-secret-value"}}`},
		stubResponse{status: http.StatusBadGateway, body: `gateway echo super-secret-value`},
		stubResponse{status: http.StatusBadRequest, body: `{"error":{"code":"validation_error","message":"value super-secret-value rejected"}}`},
	)
	var output bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &output)
	client := publicapi.New(publicapi.Options{
		BaseURL: server.URL,
		APIKey:  apiKey,
		HTTP:    &retryClient{next: server.Client(), wait: func(context.Context, time.Duration) error { return nil }},
	})
	_, _, err := client.PutTeamSecret(ctx, "NPM_TOKEN", "", publicapi.SetSecretRequest{Value: secret, Type: "runtime_secret", Repos: []string{}})
	if !publicapi.IsStatus(err, http.StatusBadRequest) || !publicapi.HasCode(err, "validation_error") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), apiKey) {
		t.Fatalf("the error shows a secret: %v", err)
	}
	sent := bodies()
	if len(sent) != 3 {
		t.Fatalf("%d requests, want the PUT retried after 503 and 502", len(sent))
	}
	for i, body := range sent {
		if !strings.Contains(body, `"value":"`+secret+`"`) {
			t.Fatalf("request %d did not replay the body: %q", i+1, body)
		}
	}
	logs := output.String()
	if !strings.Contains(logs, "Retrying Cursor API request") {
		t.Fatalf("the retries were not logged: %s", logs)
	}
	if strings.Contains(logs, secret) || strings.Contains(logs, apiKey) || strings.Contains(logs, "echo") {
		t.Fatalf("the log contains a request or response body: %s", logs)
	}
}

func TestAdminResponseSizeLimit(t *testing.T) {
	ctx := context.Background()
	pad := strings.Repeat("x", 2<<20)
	client, _ := lifecycleClient(t, func(lifecycleCall) (int, string) {
		return http.StatusOK, `{"teamId":42,"state":"custom","padding":"` + pad + `"}`
	})
	out, err := client.getModelAccessConfiguration(ctx)
	if err != nil || out.State != "custom" {
		t.Fatalf("a 2 MiB response: %v, %#v", err, out)
	}

	huge := strings.Repeat("x", adminMaxResponseBytes)
	client, _ = lifecycleClient(t, func(lifecycleCall) (int, string) {
		return http.StatusOK, `{"state":"custom","padding":"` + huge + `"}`
	})
	if _, err := client.getModelAccessConfiguration(ctx); err == nil || !strings.Contains(err.Error(), "larger than 10 MiB") {
		t.Fatalf("an oversized response: %v", err)
	}
}
