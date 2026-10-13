# Plan: Forked Cursor Terraform Provider

## Purpose

Maintain [BrazeDevelopment/terraform-provider-cursor](https://github.com/BrazeDevelopment/terraform-provider-cursor), a fork of the official Cursor Terraform provider ([cursor/terraform-provider-cursor](https://github.com/cursor/terraform-provider-cursor), Apache-2.0), as the single place to manage Cursor automations and the configuration that supports them: Cloud Agent environments, secrets, self-hosted worker pools, model access, spend controls, groups, and identity checks. The fork serves both the playground team and the production team, and every change to production Cursor configuration flows through it via reviewed Terraform.

The official provider manages automations (`cursor_platform_workflow`) and Origin resources only. Everything else in this plan is added by the fork using Cursor's public, documented APIs.

### What reaches a self-hosted run

Production automations run on self-hosted pool workers. A self-hosted run uses an environment's repository list and ignores the rest of that environment. `environment.json` (install, start, image, and the other Cursor-hosted settings) applies to Cursor-hosted agents. Environment secrets and Build Secrets are not delivered to pool workers. Team secrets reach a pool worker only when that worker is started with `--sync-dashboard-secrets` and a team admin has enabled Secret sync in the dashboard. Secret sync has no public API and stays in the settings baseline.

So F1 is the repository list (and the Cursor-hosted configuration, when a run is hosted). F3 is the secret path for pool workers. F2 configures Cursor-hosted environments. F8 selects the pool. F18 must require the `pool` label, because an empty `private_worker` block routes to any worker.

## Goals

- One provider, one repository of Terraform, one review process for all Cursor configuration that has a public API.
- Secret values never written to Terraform state or plan output.
- Plan-time guardrails that enforce the production operating model (team scope, a named worker pool, approved models) even if pipeline policy checks are bypassed.
- Minimal divergence from upstream so upstream releases can be merged routinely, and every fork feature is offered upstream so the fork can shrink or be retired.

## Non-goals

These have no public API. They remain a documented settings baseline monitored through audit-log alerts, and the fork must not call internal dashboard endpoints to manage them:

- Team settings: Require Self-Hosted Machines, network access policy and its lock, team follow-ups, computer use, long-running agents, "only admins can change secrets", Secret sync, MCP allowlist, Cursor Cloud MCP toggle.
- Service account creation, API key issuance and rotation.
- Adding team members or changing roles (removal and team-set sync exist; see F15).
- Team MCP server configuration, Protected Git Scopes, SSO and SCIM configuration.
- Listing automations (the Automations API has no list method).
- Build Secrets (the API can delete them and list them, and it cannot set or change them).
- Agent runs, which are operational activity rather than configuration.

## Design principles

1. **Public APIs only.** New features use `https://api.cursor.com` (Cloud Agents API, Admin API, Organization API) as documented at cursor.com/docs. The upstream Automations RPC is used only where upstream already uses it.
2. **Additive changes.** New functionality lives in new files and packages. Upstream files are touched only where unavoidable, and each touch is recorded in `FORK.md`:
   - resource and data source registration and new provider attributes in `provider.go`
   - new fields on `apiClient` in `client.go` (API keys and the guardrails block), so existing `*apiClient` type asserts keep working
   - exporting the retry constructor from `retry_client.go`
   The fork does not edit `resource_platform_workflow.go`.
3. **Same type names as upstream.** The provider type name stays `cursor`, so resource types remain `cursor_*` and existing configurations need no renames when switching between the official provider and the fork.
4. **Write-only secrets.** Secret values use Terraform write-only attributes and are never persisted. The release that adds them requires Terraform and OpenTofu 1.11 or later for every consumer of the provider, including stacks that only manage automations. The 1.0.0 foundation release does not contain write-only attributes.
5. **Upstream first.** Each fork feature gets an upstream issue or pull request. A fork feature is removed once upstream ships an equivalent.

## Fork setup

| Item | Decision |
| --- | --- |
| Repository | `BrazeDevelopment/terraform-provider-cursor` (existing), a GitHub fork of `cursor/terraform-provider-cursor`; add an `upstream` remote if not already configured |
| License | Keep Apache-2.0 `LICENSE` and `NOTICE`; add a statement of modifications to `NOTICE` as the license requires |
| Provider address | Private registry, for example `app.terraform.io/<org>/cursor` (HCP Terraform private registry) or an Artifactory Terraform registry |
| Provider type name | `cursor` (unchanged) |
| Versioning | Independent semantic versioning starting at `1.0.0`. Each release's changelog entry records the upstream version and commit it includes. Prerelease suffixes such as `0.3.0-<org>.1` are avoided because version constraints do not select prereleases. |
| Branching | `main` is the fork's release branch; feature branches merge through pull requests; upstream merges arrive through `sync/upstream-*` branches. `main` is never rebased. |
| Fork ledger | `FORK.md` at the repository root lists every divergence from upstream, its reason, the upstream issue or pull request, and the condition for removing it |

### Consumer migration

Existing state that references `registry.terraform.io/cursor/cursor` moves to the fork with:

```bash
terraform state replace-provider registry.terraform.io/cursor/cursor app.terraform.io/<org>/cursor
```

Consumers also change `required_providers` from `cursor/cursor` `~> 0.1` to the private registry address and an exact fork version. The reverse command retires the fork. Because type names are unchanged, no `moved` or `import` blocks are needed for existing automations.

The release that introduces F2 or F3 also requires:

```hcl
terraform {
  required_version = ">= 1.11.0"
}
```

OpenTofu supports write-only attributes from 1.11.0. OpenTofu 1.10 and Terraform 1.10 refuse to load a provider schema that contains them.

## Architecture changes

### Public API client

A new package `internal/publicapi` provides the HTTP client for `api.cursor.com`:

- Basic authentication with the API key as username, matching Cursor's documented scheme.
- Calls the upstream retry client. `newRetryClient` is unexported in package `provider`, so export a constructor from `retry_client.go` and use it here. Copying the retry loop would fork the behavior.
- Retries 429 using `Retry-After` or `X-RateLimit-Reset`. Retries idempotent methods (`GET`, `PUT`, `DELETE`) on 502, 503, and 504. The Admin API limit of 20 requests per minute per team makes this mandatory. The current client gives up when the server asks for a wait longer than two minutes; keep that ceiling and surface the error.
- Environment create and pool register are `POST`. The current client does not retry `POST` on 502, 503, or 504. A source-control failure on create returns 503 `upstream_error` with `Retry-After`. Extend the helper so a `POST` retries only when the response is 503 and carries `Retry-After`. Leave 502 and 504 on `POST` unretried, because create is not idempotent. Record the extension in `FORK.md`.
- Decodes Cursor error bodies into Terraform diagnostics. The Cloud Agents API returns `{ "error": { "code", "message" } }`. The Admin client already accepts a top-level `error` or `message` string. Include the code (for example `team_admin_required`, `secret_name_ambiguous`, `secret_name_conflict`, `secret_scope_conflict`, `feature_unavailable`, `repository_access`, `environment_name_conflict`, `validation_error`) and remediation text.
- User agent `terraform-provider-cursor/<version>+braze` on this client. Leave the Automations and Origin user agent unchanged.
- Never logs request or response bodies for secret endpoints.

Request and response types for the Cloud Agents API are generated from a pinned copy of Cursor's OpenAPI spec (`openapi/cloud-agents-openapi.yaml`, sourced from `https://cursor.com/docs-static/cloud-agents-openapi.yaml`) using `oapi-codegen`. The Admin and Organization APIs have no published OpenAPI spec, so their types are hand-written from the documentation.

### Provider configuration

`Configure` today requires `token` and, when the value starts with `key_` or `crsr_`, exchanges it for a session token before any client is built. The Cloud Agents API uses the raw key as Basic auth. A session token cannot be that key. Capture the raw `token` for the `api_key` default before the exchange.

`token` stays required for stacks that manage automations. When a stack sets `api_key`, `team_api_key`, or `pool_api_key` and does not manage automations, `token` is optional. Existing automation stacks already set `token`, so this does not change their plans.

| Attribute | Environment variable | Used by | Notes |
| --- | --- | --- | --- |
| `token` (existing) | `CURSOR_TOKEN` | Automations | Required when the configuration manages automations or Origin resources. A raw `key_` or `crsr_` value is exchanged for a session token. |
| `api_key` (new) | `CURSOR_API_KEY` | Cloud Agents API resources | Raw API key. Defaults to `token` only when `token` is still a raw `key_` or `crsr_` key, taken before exchange. Normally the Terraform service account key. |
| `team_api_key` (existing) | `CURSOR_TEAM_API_KEY` | Admin API resources and data sources | Team admin key with `admin:*` scope. Model-access writes need `models:*` or `admin:*`; `read:*` cannot call them. |
| `organization_api_key` (existing) | `CURSOR_ORGANIZATION_API_KEY` | Organization API resources and Origin `group_name` resolution | One key serves both. Use the narrowest scope that covers every resource in that stack. Narrowing it below what Origin group lookup needs breaks existing grants. |
| `organization_id` (new) | `CURSOR_ORGANIZATION_ID` | Organization API resources | Public organization ID, `org_…`. This is caller-supplied. It does not prove the key belongs to that organization; F4 is the identity check, and it cannot see a team or organization id today. |
| `pool_api_key` (new) | `CURSOR_POOL_API_KEY` | Worker pool resources | One team service-account key for every pool in the team. The pools API rejects other key types. Defaults to `api_key` when that key is a service account. |
| `guardrails` (new, block) | none | Plan-time validation | See F18. Stored on `apiClient`. |

### Guardrails

The upstream `platformWorkflowResource` implements `ValidateConfig` but not `ModifyPlan`. `ValidateConfig` does not receive provider configuration. The fork adds `ModifyPlan` in `internal/provider/fork_guardrails_platform_workflow.go`. `Configure` in the upstream resource file already stores `*apiClient` on the resource, so `ModifyPlan` reads the guardrails from that client. Adding the field is the `client.go` touch above. The `ProviderData` type stays `*apiClient`.

## Feature backlog

Priority P0 is required for the production operating model, P1 brings team controls under code, and P2 is optional.

Identity assertions (F4 and F5) are `lifecycle` preconditions on a managed resource. A `check` block is a warning. It does not fail `plan` or `apply`.

### P0: environments, secrets, identity checks, worker pools

**F1. Resource `cursor_environment`**
- API: `POST /v1/environments`, `GET /v1/environments/{id}`, `PATCH /v1/environments/{id}`, `DELETE /v1/environments/{id}`.
- Arguments: `owner` (`team` or `personal`), `name`, `repos` (list of `{ url }`), `environment_json` (JSON string using the `.cursor/environment.json` schema).
- Computed: `id`. This UUID is the value automations store in `environment_public_id` (`AgentOptions.environment_public_id` is that public UUID).
- `owner` and `repos` force replacement. `PATCH` accepts only `name` and `environmentJson`. Sending `repos` or `owner` is rejected (`additionalProperties: false`). Replacing an environment changes its id, so automations that reference it must move in the same apply. `create_before_destroy` is not available: the owner cannot have two environments with the same name (`409 environment_name_conflict`).
- `environment_json` is normalized so key order and whitespace do not drift. When `name` and `environment_json` change together, send both in one `PATCH`; the API applies them together. `PATCH` returns 204, so refresh with `GET`.
- A read that returns `repoFile` means the environment loads its configuration from a file in a repository. The provider returns a diagnostic and does not `PATCH`. That file is managed in git.
- A 404 on read removes the resource from state. Import by environment ID.
- Done when: create, update, rename, delete and import pass acceptance tests; no drift on a second plan; changing `repos` proposes replacement; a `repoFile` response produces a diagnostic; `feature_unavailable` and `validation_error` produce clear diagnostics.

**F2. Resource `cursor_environment_secret`**
- API: `PUT /v1/environments/{id}/secrets/{name}`, `DELETE /v1/environments/{id}/secrets/{name}`, `GET /v1/environments/{id}/secrets` for read. One list item is one version. A name can have several versions, each covering different repositories.
- The Terraform resource is one version, identified by `secret_version_id`, not one name.
- Arguments: `environment_id` and `name` (both force replacement), `type` (`runtime_secret` or `environment_variable`), `repos`, `value_wo` (write-only), `value_wo_version` (number).
- `repos` is required in the schema so create and update always send it. An empty list means every repository. Omitting `repos` on `PUT` would keep the previous set, which is the wrong Terraform behavior. State stores the list the API returns: lowercase, sorted, reduced to `host/owner/repo`. Plan comparison reduces the configured values the same way. An acceptance test covers `acme/api`, an `https://` URL, and a `git@` URL and expects no drift after apply.
- Every `PUT` requires `value`, including a change of only `type` or `repos`. Read `value_wo` from the configuration on create and on every update. `value_wo_version` is what marks the value itself as changed when `type` and `repos` are unchanged. Refresh can see deletion and changes to type or repos, and it cannot see value changes.
- Computed: `secret_version_id`, sent as `?id=` on update and delete. A `201` can omit `id` until the version is readable; follow the `PUT` with `GET` and match the version. If the name is still ambiguous, return the diagnostic and do not guess.
- Import is `<environment_id>/<name>/<secret_version_id>`. An import id without a version fails when the name has several versions, and the diagnostic lists those version ids.
- Schema rejects `build_secret`. A read that returns `build_secret` produces a diagnostic: the API can delete a Build Secret and cannot update one.
- Names that differ only by case return `409 secret_name_conflict`.
- Requires Terraform or OpenTofu 1.11 or later. That requirement applies to the whole provider once this resource ships.
- Done when: an acceptance test asserts the secret value appears in neither the state file nor the plan JSON; repo spelling does not drift; `403 team_admin_required` produces a diagnostic explaining the team setting.

**F3. Resource `cursor_team_secret`**
- API: `PUT /v1/team/secrets/{name}`, `DELETE /v1/team/secrets/{name}`, `GET /v1/team/secrets`.
- Same schema and behavior as F2 without `environment_id`. This is the secret production pool workers can receive, and only when Secret sync is on and the worker was started with `--sync-dashboard-secrets`.
- Import is `<name>/<secret_version_id>`.

**F4. Data source `cursor_api_key_info`**
- API: `GET /v1/me`, called with `api_key`.
- Attributes: `api_key_name` and `created_at` are always set. `user_id`, `user_email`, `user_first_name`, and `user_last_name` are set for a user-scoped key and null for a service account.
- A service account, which is the production Terraform key, returns only `apiKeyName` and `createdAt`. There is no team id and no user email. The production precondition compares `api_key_name` to the expected key name. It cannot prove which team the key belongs to until `GET /v1/me` grows a team id (open question 4).
- The precondition sits on a managed resource in each stack. It does not use a `check` block.

**F5. Data source `cursor_team_members`**
- API: Admin API `GET /teams/members`.
- Attributes: list of `{ id, email, name, role, is_removed }`. `role` is `member` or `owner`. There is no `admin` value.
- Use: a `lifecycle` precondition in the production stack asserts that every member with `is_removed = false` is on the approved list and has `role = "owner"` when that is the production rule. The same precondition is access-review evidence. A `check` block does not fail the plan.

**F6. Data sources `cursor_environment`, `cursor_environments`, `cursor_environment_active_build`**
- API: `GET /v1/environments/{id}`, `GET /v1/environments`, `GET /v1/environments/{id}/builds/active`, and `GET /v1/environments/{id}/builds/{buildId}` for status.
- The list is cursor-paginated (`limit` default 20, max 100). Keep requesting until `nextCursor` is absent. It is omitted, not null, on the last page. A page can be shorter than `limit`. An environment updated during the walk can move to the front, so a later page can skip it or repeat another. The data source documents that and still returns the walk it completed.
- List items omit `environmentJson` and `versionId`. The single-environment data source calls `GET` by id for those.
- The active-build payload is `{ type = "build", build_id = "..." }` or `{ type = "universal_image" }`. It has no status. `universal_image` means the agent boots Cursor's default image. Health is the `status` of `GET /v1/environments/{id}/builds/{buildId}` (`IN_PROGRESS`, `SUCCEEDED`, `FAILED`, `CANCELLED`, `SKIPPED`).
- A precondition on an automation may require `type = "build"` and `status = "SUCCEEDED"`. A configuration change starts that build asynchronously (`trigger = CONFIG_CHANGE`), so the precondition cannot pass in the same apply that changed the environment. Apply the environment first, then the automation.

**F7. Data source `cursor_models`**
- API: `GET /v1/models`.
- This catalog is the set of `model.id` values accepted by `POST /v1/agents` (for example `composer-2`, `claude-4.6-sonnet-thinking`), including `parameters` and `variants`.
- Do not use it to validate automation `model` or `model_selection.model_id`, or Admin model-access ids (those look like `anthropic` / `claude-opus-4-6`), until Cursor confirms the catalogs are the same. See open question 6. F18 uses a static allow-list in the meantime.

**F8. Resource `cursor_private_worker_pool` and data sources `cursor_private_worker_pools`, `cursor_private_workers_summary`**
- API: `POST /v0/private-workers/pools` (register), `DELETE /v0/private-workers/pools` (deregister), `GET /v0/private-workers/pools`, `GET /v0/private-workers/summary`.
- Register returns `{ "registered": true }` and no id. The Terraform id is `team/<pool_name>` for an any-repo pool and `team/<pool_name>/<repo_owner>/<repo_name>` for a repo-backed pool (`user/...` when `scope = "user"`).
- Arguments: `scope` (`team` or `user`; the register body uses these values), `pool_name`, `worker_ready_timeout_seconds`. `repo_owner` and `repo_name` are optional and must be set together. `repo_url` is optional and requires both. Omitting the repo arguments registers an any-repo pool, which routes on the pool name alone.
- The list filter is a different enum: `team_pool`, `personal`, or `all`. The data source maps resource scope `team` to query `scope=team_pool`.
- Delete sends query parameters `scope`, `pool_name`, and, for a repo-backed pool, `repo_owner` and `repo_name`. Deregister is a soft-delete: the pool leaves pickers, and workers already connected stay connected.
- There is no update endpoint. Every argument change forces replacement. Replacement removes the pool from pickers until the new one is registered. Document that for `worker_ready_timeout_seconds` in particular.
- Authentication: `pool_api_key`, one team service-account key for every pool in the team.
- Automations select the pool with `private_worker.labels.pool = cursor_private_worker_pool.<name>.pool_name`. The `repo` label is reserved; Cursor sets it from the run, and configurations must not set it.
- Import uses the same id as state.
- Risk: this is a v0 API and carries higher change risk; it is covered by the contract monitor.

### P1: team controls in the Admin and Organization APIs

**F9. Resources `cursor_model_access_configuration`, `cursor_model_access_provider`, `cursor_model_access_model`**
- API: Admin API `GET/PUT /teams/model-access/configuration`, `GET/PUT /teams/model-access/providers/:provider`, and `GET/PUT /teams/model-access/providers/:provider/models/:model`. The Organization API bulk endpoints can be added later for multi-team management.
- These ids are Admin catalog ids (`anthropic`, `claude-opus-4-6`). They are a different space from automation model slugs and from `GET /v1/models` until open question 6 is answered.
- Provider and model reads and writes return 409 while configuration `state` is `unrestricted` or `legacy`. The configuration resource's first `PUT` turns the policy on and seeds the catalog. The provider and model resources, on that 409, diagnose that the configuration resource has to be applied first.
- Destroying the configuration resource sends `PUT { "state": "unrestricted" }`, which clears the custom policy. The resource defaults `deletion_protection` to true.
- Organization Groups can still allow a model the team policy disables. This API is the team baseline. The effective allow is the union.
- The current Admin API docs do not mark these routes as preview. They are available when model access control is enabled for the team. Treat the schema as still able to change, and cover it with the contract monitor.

**F10. Resource `cursor_user_spend_limit`**
- API: `POST /teams/user-spend-limit` with `userEmail` and `spendLimitDollars`. Enterprise only.
- `spend_limit_dollars` is an integer. `0` sets a zero-dollar limit. Destroy, and an explicit clear, send `null`, which removes the limit.
- There is no dedicated read. Refresh reads `monthlyLimitDollars` from `POST /teams/spend` (paginated). `null` there means no limit. Ignore `hardLimitOverrideDollars` for this resource; `0` on that field means no override and is a different setting.
- The bulk route `POST /teams/user-spend-limits` is preview and returns 403 for teams that are not enabled. Use the single-user route. Revisit bulk only if a stack is too large for one request per user at 250 requests per minute.

**F11. Resources `cursor_team_directory_group`, `cursor_team_directory_group_members`**
- API: Admin API `/teams/directory-groups`. Group id is `team_group_…`. These ids are not Organization Group `g_` / `grp_` ids or Billing Group `group_…` ids.
- `monthly_spending_limit_dollars` is one limit on the group, applied to each member. It is not a per-member argument. `null` in state means no limit; clearing it sends `clearMonthlySpendingLimitDollars = true` and omits the dollar field. `0` is a zero-dollar limit.
- The members resource owns the member set of one manual group (`bulk-add` and `bulk-remove`, up to 100 user ids per call). Delete of the group requires the group to be empty, so the members resource is destroyed first.
- The API can create manual groups only. SCIM-synced groups are created by the identity provider. Member add and remove return 400 on a SCIM-synced group, and delete returns 400 while a SCIM mapping exists. When SCIM owns the group, use F5 preconditions and do not manage membership with F11.

**F12. Resources `cursor_organization_group`, `cursor_organization_group_members`**
- API: Organization API `/organizations/groups`. `id` uses the `g_` prefix. Responses also include `publicId` with the `grp_` prefix.
- The resource exports `public_id`. Origin grants and the upstream `resolveOrganizationGroupID` helper use `publicId`, so grant resources reference `public_id`, not `id`.
- Upstream already resolves organization groups by name for Origin grants. New methods live in a new file on `apiClient` so `admin_client.go` stays untouched.

**F13. Resource `cursor_repo_blocklist`**
- API: `GET /settings/repo-blocklists/repos`, `POST /settings/repo-blocklists/repos/upsert`, `DELETE /settings/repo-blocklists/repos/:repoId`.
- One resource per repository URL. Upsert replaces patterns for the repositories in the request and leaves every other repository alone. Delete uses the `repo_…` id from the read.
- Blocklists apply to the editor and CLI, not to Cloud Agents or automations. This is included for central management, not as a control for automations.

### P2: optional

**F14. Resources `cursor_billing_group`, `cursor_billing_group_members`**
- API: Admin API `/teams/groups`. Ids use the `group_…` prefix, which is neither `team_group_…` nor `grp_`.

**F15. Resource `cursor_organization_user_teams`**
- API: Organization API `POST /organizations/team-memberships/sync`.
- Semantics: authoritative for apply. Each resource owns one user's complete set of teams, and teams removed from the set are removed from the user. Roles cannot be set.
- Destroy removes the resource from state and does not call the API. A destroy that synced an empty team list would remove the user from every team.
- Adopt only if the identity provider is not already authoritative for team membership. When SCIM manages membership, use F5 preconditions instead, so access has a single source of truth.

**F16. Tool `cmd/cursor-inventory`** (shipped in the repository, not part of the provider binary)
- Calls `POST /teams/filtered-usage-events` with `automationId=*` and reads `GET /teams/audit-logs`. Usage events are aggregated hourly; poll at most once per hour. The tool compares automation ids that appeared in the window with Terraform state and reports automations that ran and are not managed.
- An automation that did not run in the window is invisible to this tool. It is not a complete inventory.
- The job runs in the automations repository, which holds the production Admin key. The provider repository's CI keeps playground keys only.
- Exports audit events to the SIEM when native audit-log streaming is not enabled.

**F17. Data source `cursor_audit_events`**
- Only if a plan-time precondition on recent audit events proves useful. Otherwise F16 covers this. A `check` block would not fail the plan.

### Fork-only feature

**F18. Provider `guardrails` block**

```hcl
provider "cursor" {
  guardrails {
    allowed_automation_scopes = ["team"]
    require_pool_label        = true
    allowed_models            = ["<automation-catalog-model-id>"]
    allowed_repo_owners       = ["<org>"]
    deny_pr_approval          = true
  }
}
```

Enforced in `ModifyPlan` for `cursor_platform_workflow`, and in validation for new resources where the same rule applies (F1 repository owners, F8 pool name). Violations fail the plan with a diagnostic naming the rule. Unknown values wait; they do not pass.

- `allowed_automation_scopes`: an unset `scope` is `user`. `team` and `team_editable` both run as the team service account and are different scopes. List the scopes production uses. `team_visible` and `team_editable_user` run as the creator.
- `require_pool_label`: `private_worker` is present and `labels["pool"]` is a non-empty string. `private_worker {}` routes to any worker and fails this rule. A configured `repo` label fails the plan; Cursor sets `repo` itself.
- `allowed_models`: when `model_selection` is set, compare `model_selection.model_id`. Otherwise compare `model`. When both are unset, fail the plan. The server assigns a default only after apply, so a plan-time check cannot see it. This list is the automation catalog. It is not filled from `GET /v1/models` or from Admin model-access ids unless open question 6 says they match.
- `allowed_repo_owners`: match the owner segment of trigger `repos`, trigger `orgs`, `git_push.repo`, and `git_repo`. Those fields use `org/repo`, `github.com/org/repo`, and full URLs. Compare the owner, not the raw string. Environment `repos` on F1 use the same owner check.
- `deny_pr_approval`: reject `action.approve_pr` and `action.pr_comment.allow_approve = true`. `approve_pr` is deprecated. `allow_approve` is the switch the server honors.

This is defense in depth alongside pipeline policy checks. It stays fork-only unless upstream wants it. It constrains automations this provider manages. It does not constrain dashboard or API launches, and it does not read the Require Self-Hosted Machines setting.

## Testing

- **Unit tests** follow upstream's pattern of in-process HTTP mock servers. Cover every create, read, update, delete and import path, plus `409 secret_name_ambiguous`, `409 secret_name_conflict`, `409 secret_scope_conflict`, `409 environment_name_conflict`, `403 team_admin_required`, `403 feature_unavailable`, `403 repository_access`, `400 validation_error`, 404 drift, a `201` secret response that omits `id`, canonical secret `repos`, environment `PATCH` rejecting `repos`, pool register returning only `{registered: true}`, and 429 retry. Include a `POST` 503 with `Retry-After` for environment create.
- **Acceptance tests** use `terraform-plugin-testing` with `TF_ACC=1` against the playground team, using dedicated playground keys stored as CI secrets. Object names carry a `tfacc-` prefix and the CI run id, and a sweeper removes leftovers. The job is serialized to one run at a time: parallel runs share the playground team and its 20 requests-per-minute Admin budget. They run on every merge to `main` and nightly.
- **Secret leakage test:** acceptance tests assert that secret values appear in neither `terraform show -json` state output nor the saved plan JSON.
- **Contract monitor:** a nightly job fetches the Cloud Agents OpenAPI spec and diffs it against the pinned copy. That diff fails the job. A hash of the Admin and Organization API documentation pages opens a triage issue and does not fail the job, because unrelated page edits change the hash.
- **Upstream compatibility:** every upstream sync runs the full unit and acceptance suites before merge.

## Upstream synchronization

1. A weekly scheduled workflow fetches `upstream/main`, merges it into a `sync/upstream-<date>` branch, runs CI, and opens a pull request. Merge conflicts are resolved by the provider owner. `provider.go` and `client.go` are the files both sides edit.
2. Each upstream release is merged and included in the next fork release, and the changelog records the upstream version.
3. For every fork feature, an issue or pull request is opened upstream and linked in `FORK.md`.
4. When upstream ships an equivalent resource with the same type name and compatible schema, the fork drops its implementation in the next sync. If the schema differs, the fork ships a deprecation release and a migration guide using `removed` and `import` blocks.
5. **Retirement criteria:** when upstream covers F1 through F8 (or Cursor ships equivalent functionality), consumers move back to `cursor/cursor` with `terraform state replace-provider`, and the fork is archived.

## Security and compliance controls

The fork writes production configuration, so it is in scope for SOX IT general controls (program change), SOC 2 CC8.1, and ISO/IEC 27001 Annex A 8.25, 8.28 and 8.32.

- Branch protection on `main`: pull requests only, two approvals, CODEOWNERS (platform and security), required status checks, signed commits, no force pushes.
- Pin all GitHub Actions to commit SHAs (upstream already does this) and keep dependencies current with Renovate or Dependabot.
- Static analysis: `go vet`, `golangci-lint`, `gosec`, `govulncheck`.
- Releases: protected `v*` tags, a release job behind an approval-gated deployment environment, GPG-signed checksums with the signing key held in a KMS- or HSM-backed secret, an SBOM generated by GoReleaser, and build provenance attestations.
- Distribution: private registry with read access limited to CI and platform engineers.
- Consumers pin exact provider versions and commit `.terraform.lock.hcl` with hashes for every platform CI uses.
- Credentials: the provider repository's CI holds playground keys only. Production keys exist only in the automations repository's protected apply job. F16 runs there.
- All secret-bearing attributes are marked sensitive or write-only, and the client masks them in logs.

## Work breakdown

| Phase | Work | Exit criteria |
| --- | --- | --- |
| 0. Decisions and setup | Confirm `BrazeDevelopment/terraform-provider-cursor` is current with upstream and record the base commit; choose the registry and namespace; provision the GPG signing key; configure branch protection, CODEOWNERS and CI; pin the OpenAPI spec; create `FORK.md`; create playground acceptance-test keys | Fork repository protected; CI green on unchanged upstream code |
| 1. Foundation | Implement the `publicapi` client and new provider configuration, with no write-only attributes; release `1.0.0`, functionally identical for existing resources, to the private registry; migrate the playground and production automation stacks with `state replace-provider` and an updated `required_providers` version | Both stacks plan with no changes against the fork |
| 2. P0 features | F1 through F8 with docs, examples, unit and acceptance tests. Raise consumer `required_version` to `>= 1.11.0` in the release that adds F2 | Production environments, team secrets, and pools managed by Terraform; F4 and F5 preconditions active in the production stack; environment secrets used for Cursor-hosted environments |
| 3. Guardrails and tooling | F18 guardrails, F16 inventory tool, contract monitor | Guardrails enabled in the production stack with `require_pool_label` and `deny_pr_approval`; nightly inventory report flowing to the SIEM from the automations repository |
| 4. P1 features | F9 through F13 | Model access and spend limits managed as code in production, with configuration `deletion_protection` left on |
| 5. Steady state | Weekly upstream sync, upstream contributions, quarterly fork review | `FORK.md` current; at least one fork feature proposed upstream |

P2 features are scheduled only when a concrete need appears.

## Ownership and maintenance

- **Owner:** platform engineering, with a named primary and backup maintainer.
- **Recurring duties:** review the weekly upstream sync, triage contract-monitor alerts, cut releases, keep `FORK.md` current, and run a quarterly review of the fork delta and retirement criteria.
- **API breaking change runbook:** consumers stay on their pinned version; the maintainer patches the client, adds a regression test, releases, and the change rolls through playground before production.
- **Incident runbook:** if a provider release misbehaves, revert the version pin in the automations repository and re-plan. State remains compatible because type names and schemas only change through deprecation releases.

## Risks

| Risk | Mitigation |
| --- | --- |
| Cloud Agents API is beta and worker pools use a v0 API, so schemas may change. Model-access routes are ungated only when the feature is enabled, and the bulk spend route is preview | Contract monitor, exact version pins, playground-first rollout |
| Upstream changes conflict with fork changes | Additive design, weekly syncs, `FORK.md`; expected conflicts are limited to `provider.go` and `client.go` |
| Upstream later adds a resource with the same name but a different schema | Coordinate through upstream issues before building; deprecation and migration path |
| "Only admins can change secrets" blocks service account keys (`403 team_admin_required`). The members API reports those people as `role = "owner"` | Leave the setting off in the production team, and alert on secret events from any actor other than the Terraform service account |
| Secret sync is off, or workers are started without `--sync-dashboard-secrets`, so team secrets never reach pool workers | Settings baseline and worker startup docs. The provider cannot read this setting |
| F18 allow-lists the wrong model catalog | Static automation catalog ids until open question 6 is answered |
| Admin and Organization keys carry broad scopes | Narrowest scope that still covers Origin group lookup and the resources in the stack; exposed only to the protected apply job; rotated on a schedule |
| Write-only attributes require Terraform and OpenTofu 1.11 or later for the entire provider | Keep them out of 1.0.0. Raise `required_version` in the F2 release |
| Acceptance tests share one playground team and a 20 request-per-minute Admin budget | One run at a time, run-id suffixes, retry on 429 |

## Open questions for Cursor

1. Will environment, secret, worker pool and Admin API resources be added to the official provider, and are contributions accepted?
2. Will the worker pool endpoints move from v0 to v1, and on what compatibility terms? They are still `/v0/private-workers/pools`.
3. Model-access routes are documented without a preview label, and they 409 until a custom policy exists. Is that schema considered stable, and can a team policy exclude models that an Organization Group allows?
4. Can `GET /v1/me` return the key's team ID? Today a service-account key returns only `apiKeyName` and `createdAt`, so a production stack can assert the key name and cannot assert the team.
5. Is read access to team settings (self-hosted requirement, network policy, secrets restriction, Secret sync, follow-ups) planned for the Admin API, so the settings baseline can be checked at plan time?
6. Are `GET /v1/models` ids, automation `model_selection.model_id` values, and Admin model-access ids (`claude-opus-4-6`) the same catalog? Until that is confirmed, F7 does not gate automations and F18 uses a static allow-list.

## Resolved

- A user spend limit is cleared with `POST /teams/user-spend-limit` and `spendLimitDollars: null`. `0` sets a zero-dollar limit. The current value is `monthlyLimitDollars` on `POST /teams/spend`.
- OpenTofu 1.11.0 supports write-only attributes. OpenTofu 1.10 does not, and a provider that declares them fails to load there.
