# Plan: Forked Cursor Terraform Provider

## Purpose

Maintain [BrazeDevelopment/terraform-provider-cursor](https://github.com/BrazeDevelopment/terraform-provider-cursor), a fork of the official Cursor Terraform provider ([cursor/terraform-provider-cursor](https://github.com/cursor/terraform-provider-cursor), Apache-2.0), as the single place to manage Cursor automations and the configuration that supports them: Cloud Agent environments, secrets, self-hosted worker pools, model access, spend controls, groups, and identity checks. The fork serves both the playground team and the production team, and every change to production Cursor configuration flows through it via reviewed Terraform.

The official provider manages automations (`cursor_platform_workflow`) and Origin resources only. Everything else in this plan is added by the fork using Cursor's public, documented APIs.

## Goals

- One provider, one repository of Terraform, one review process for all Cursor configuration that has a public API.
- Secret values never written to Terraform state or plan output.
- Plan-time guardrails that enforce the production operating model (team scope, self-hosted workers, approved models) even if pipeline policy checks are bypassed.
- Minimal divergence from upstream so upstream releases can be merged routinely, and every fork feature is offered upstream so the fork can shrink or be retired.

## Non-goals

These have no public API. They remain a documented settings baseline monitored through audit-log alerts, and the fork must not call internal dashboard endpoints to manage them:

- Team settings: Require Self-Hosted Machines, network access policy and its lock, team follow-ups, computer use, long-running agents, "only admins can change secrets", MCP allowlist, Cursor Cloud MCP toggle.
- Service account creation, API key issuance and rotation.
- Adding team members or changing roles (removal and team-set sync exist; see F15).
- Team MCP server configuration, Protected Git Scopes, SSO and SCIM configuration.
- Listing automations (the Automations API has no list method).
- Build Secrets (the API cannot set them).
- Agent runs, which are operational activity rather than configuration.

## Design principles

1. **Public APIs only.** New features use `https://api.cursor.com` (Cloud Agents API, Admin API, Organization API) as documented at cursor.com/docs. The upstream Automations RPC is used only where upstream already uses it.
2. **Additive changes.** New functionality lives in new files and packages. Upstream files are touched only where unavoidable: resource and data source registration in `provider.go`, and new provider configuration attributes. This keeps upstream merges mechanical.
3. **Same type names as upstream.** The provider type name stays `cursor`, so resource types remain `cursor_*` and existing configurations need no renames when switching between the official provider and the fork.
4. **Write-only secrets.** Secret values use Terraform write-only attributes and are never persisted.
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

The reverse command retires the fork. Because type names are unchanged, no `moved` or `import` blocks are needed for existing automations.

## Architecture changes

### Public API client

A new package `internal/publicapi` provides the HTTP client for `api.cursor.com`:

- Basic authentication with the API key as username, matching Cursor's documented scheme.
- Reuses upstream's retry client: retries 429 using `Retry-After` or `X-RateLimit-Reset`, and retries idempotent requests on 502, 503 and 504. The Admin API limit of 20 requests per minute per team makes this mandatory.
- Decodes Cursor error bodies into Terraform diagnostics that include the API error `code` (for example `team_admin_required`, `secret_name_ambiguous`, `feature_unavailable`, `repository_access`) with remediation text.
- User agent `terraform-provider-cursor/<version>+braze`.
- Never logs request or response bodies for secret endpoints.

Request and response types for the Cloud Agents API are generated from a pinned copy of Cursor's OpenAPI spec (`openapi/cloud-agents-openapi.yaml`, sourced from `https://cursor.com/docs-static/cloud-agents-openapi.yaml`) using `oapi-codegen`. The Admin and Organization APIs have no published OpenAPI spec, so their types are hand-written from the documentation.

### Provider configuration

| Attribute | Environment variable | Used by | Notes |
| --- | --- | --- | --- |
| `token` (existing) | `CURSOR_TOKEN` | Automations | Unchanged |
| `api_key` (new) | `CURSOR_API_KEY` | Cloud Agents API resources | Defaults to `token` when `token` is a raw `key_` or `crsr_` key. Normally the Terraform service account key. |
| `team_api_key` (existing) | `CURSOR_TEAM_API_KEY` | Admin API resources and data sources | Team admin key with `admin:*` scope |
| `organization_api_key` (existing) | `CURSOR_ORGANIZATION_API_KEY` | Organization API resources | Prefer the narrowest scope that works (`members:*` rather than `admin:*`) |
| `organization_id` (new) | `CURSOR_ORGANIZATION_ID` | Organization API resources | Public organization ID, `org_…` |
| `pool_api_key` (new) | `CURSOR_POOL_API_KEY` | Worker pool resources | The pool's service account key; the pools API rejects other key types |
| `guardrails` (new, block) | none | Plan-time validation | See F18 |

### Guardrails without editing upstream files

The upstream `platformWorkflowResource` implements `ValidateConfig` but not `ModifyPlan`. The fork adds `ModifyPlan` in a separate file (`internal/provider/fork_guardrails_platform_workflow.go`), so guardrails are enforced without changing upstream's resource file.

## Feature backlog

Priority P0 is required for the production operating model, P1 brings team controls under code, and P2 is optional.

### P0: environments, secrets, identity checks, worker pools

**F1. Resource `cursor_environment`**
- API: `POST /v1/environments`, `GET /v1/environments/{id}`, `PATCH /v1/environments/{id}`, `DELETE /v1/environments/{id}`.
- Arguments: `owner` (`team` or `personal`, forces replacement), `name`, `repos` (list of `{ url }`), `environment_json` (JSON string using the `.cursor/environment.json` schema; normalized so key order and whitespace do not cause drift).
- Computed: `id`.
- Behavior: `PATCH` replaces the whole configuration, so the provider always sends the full `environment_json`. A 404 on read removes the resource from state. Import by environment ID.
- Done when: create, update, rename, delete and import pass acceptance tests; no drift on a second plan; `feature_unavailable` produces a clear diagnostic.

**F2. Resource `cursor_environment_secret`**
- API: `PUT /v1/environments/{id}/secrets/{name}`, `DELETE /v1/environments/{id}/secrets/{name}`, `GET /v1/environments/{id}/secrets` for read.
- Arguments: `environment_id` and `name` (both force replacement), `type` (`runtime_secret` or `environment_variable`), `repos` (optional), `value_wo` (write-only, sensitive), `value_wo_version` (number; changing it sends the new value).
- Computed: `secret_version_id`, used as `?id=` on update and delete so names with several versions never hit `409 secret_name_ambiguous`.
- Behavior: refresh can detect deletion and changes to type or repos, but not value changes, so rotation is driven by `value_wo_version`. Import as `<environment_id>/<name>`.
- Requires Terraform 1.11 or later for write-only attributes. Confirm OpenTofu support before adopting it there.
- Done when: an acceptance test asserts the secret value appears in neither the state file nor the plan JSON; `403 team_admin_required` produces a diagnostic explaining the team setting.

**F3. Resource `cursor_team_secret`**
- API: `PUT /v1/team/secrets/{name}`, `DELETE /v1/team/secrets/{name}`, `GET /v1/team/secrets`.
- Same schema and behavior as F2 without `environment_id`. Import by name.

**F4. Data source `cursor_api_key_info`**
- API: `GET /v1/me`.
- Attributes: `api_key_name`, `user_id`, `user_email`, `created_at`.
- Use: a `check` block in each stack asserts the expected key is in use, which prevents applying a production configuration with a playground key or the reverse.

**F5. Data source `cursor_team_members`**
- API: Admin API `GET /teams/members`.
- Attributes: list of `{ id, email, name, role, is_removed }`.
- Use: `check` blocks in the production stack assert that every active member is on the approved list and holds the admin role. Unexpected membership fails the plan and becomes access-review evidence.

**F6. Data sources `cursor_environment`, `cursor_environments`, `cursor_environment_active_build`**
- API: `GET /v1/environments`, `GET /v1/environments/{id}`, `GET /v1/environments/{id}/builds/active`.
- Use: reference environments managed outside Terraform, and gate automation enablement on a healthy active build.

**F7. Data source `cursor_models`**
- API: `GET /v1/models`.
- Use: validate that automation model IDs exist and appear on the approved list.

**F8. Resource `cursor_private_worker_pool` and data sources `cursor_private_worker_pools`, `cursor_private_workers_summary`**
- API: `POST /v0/private-workers/pools` (register), `DELETE /v0/private-workers/pools` (deregister), `GET /v0/private-workers/pools`, `GET /v0/private-workers/summary`.
- Arguments: `scope` (`team`), `pool_name`, `repo_owner`, `repo_name`, `repo_url`, `worker_ready_timeout_seconds`. With no update endpoint, every change forces replacement.
- Authentication: the pool's service account key (`pool_api_key`).
- Use: automations reference `cursor_private_worker_pool.<name>.pool_name` in `private_worker.labels`, so routing to a pool that does not exist fails at plan time.
- Risk: this is a v0 API and carries higher change risk; it is covered by the contract monitor.

### P1: team controls in the Admin and Organization APIs

**F9. Resources `cursor_model_access_configuration`, `cursor_model_access_provider`, `cursor_model_access_model`**
- API: Admin API `GET/PUT /teams/model-access/configuration`, provider and model update endpoints. The Organization API bulk endpoints can be added later for multi-team management.
- Use: the approved model list (zero-data-retention models only) becomes code instead of a dashboard setting.
- Risk: the Admin API marks model access as preview.

**F10. Resource `cursor_user_spend_limit`**
- API: Admin API set user spend limit, and the bulk variant (preview).
- Behavior: confirm with Cursor how a limit is cleared before implementing delete.

**F11. Resources `cursor_team_directory_group`, `cursor_team_directory_group_members`**
- API: Admin API `/teams/directory-groups` create, read, update, delete, plus member add and remove.
- Arguments include `monthly_spending_limit_dollars` per member.

**F12. Resources `cursor_organization_group`, `cursor_organization_group_members`**
- API: Organization API `/organizations/groups` create, read, update, delete, plus member add and remove.
- Upstream already resolves organization groups by name for Origin grants; the client code is shared.

**F13. Resource `cursor_repo_blocklist`**
- API: Admin API get, upsert and delete repo blocklists.
- Note: blocklists apply to the editor and CLI, not to Cloud Agents or automations. This is included for central management, not as a control for automations.

### P2: optional

**F14. Resources `cursor_billing_group`, `cursor_billing_group_members`**
- API: Admin API `/teams/groups` create, update, delete, plus member add and remove.

**F15. Resource `cursor_organization_user_teams`**
- API: Organization API `POST /organizations/team-memberships/sync`.
- Semantics: authoritative. Each resource owns one user's complete set of teams, and teams not listed are removed. Roles cannot be set.
- Adopt only if the identity provider is not already authoritative for team membership. When SCIM manages membership, use F5 checks instead, so access has a single source of truth.

**F16. Tool `cmd/cursor-inventory`** (shipped in the repository, not part of the provider binary)
- Pulls Admin API usage events filtered with `automationId=*` and audit events, compares the automation IDs seen against Terraform state, and reports automations that ran but are not managed (unmanaged automation detection).
- Exports audit events to the SIEM when native audit-log streaming is not enabled.

**F17. Data source `cursor_audit_events`**
- Only if a plan-time check on recent audit events proves useful. Otherwise F16 covers this.

### Fork-only feature

**F18. Provider `guardrails` block**

```hcl
provider "cursor" {
  guardrails {
    allowed_automation_scopes = ["team"]
    require_private_worker    = true
    allowed_models            = ["<approved-model-id>"]
    allowed_repo_owners       = ["<org>"]
    deny_actions              = ["approve_pr"]
  }
}
```

- Enforced in `ModifyPlan` for `cursor_platform_workflow` and validation hooks for new resources. Violations fail the plan with a diagnostic naming the rule.
- This is defense in depth alongside pipeline policy checks. It stays fork-only unless upstream wants it.

## Testing

- **Unit tests** follow upstream's pattern of in-process HTTP mock servers. Cover every create, read, update, delete and import path, plus the error codes `409 secret_name_ambiguous`, `403 team_admin_required`, `403 feature_unavailable`, `403 repository_access`, 404 drift, and 429 retry.
- **Acceptance tests** use `terraform-plugin-testing` with `TF_ACC=1` against the playground team, using dedicated playground keys stored as CI secrets. Test objects are prefixed `tfacc-`, and a sweeper removes leftovers. They run on every merge to `main` and nightly.
- **Secret leakage test:** acceptance tests assert that secret values appear in neither `terraform show -json` state output nor the saved plan JSON.
- **Contract monitor:** a nightly job fetches the Cloud Agents OpenAPI spec and diffs it against the pinned copy, and hashes the Admin and Organization API documentation pages. Any change opens an issue for triage.
- **Upstream compatibility:** every upstream sync runs the full unit and acceptance suites before merge.

## Upstream synchronization

1. A weekly scheduled workflow fetches `upstream/main`, merges it into a `sync/upstream-<date>` branch, runs CI, and opens a pull request. Merge conflicts are resolved by the provider owner.
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
- Credentials: the provider repository's CI holds playground keys only. Production keys exist only in the automations repository's protected apply job.
- All secret-bearing attributes are marked sensitive or write-only, and the client masks them in logs.

## Work breakdown

| Phase | Work | Exit criteria |
| --- | --- | --- |
| 0. Decisions and setup | Confirm `BrazeDevelopment/terraform-provider-cursor` is current with upstream and record the base commit; choose the registry and namespace; provision the GPG signing key; configure branch protection, CODEOWNERS and CI; pin the OpenAPI spec; create `FORK.md`; create playground acceptance-test keys | Fork repository protected; CI green on unchanged upstream code |
| 1. Foundation | Implement the `publicapi` client and new provider configuration; release `1.0.0`, functionally identical to the upstream base, to the private registry; migrate the playground and production automation stacks with `state replace-provider` | Both stacks plan with no changes against the fork |
| 2. P0 features | F1 through F8 with docs, examples, unit and acceptance tests | Production environments, secrets and pools managed by Terraform; `check` blocks for F4 and F5 active in the production stack |
| 3. Guardrails and tooling | F18 guardrails, F16 inventory tool, contract monitor | Guardrails enabled in the production stack; nightly inventory report flowing to the SIEM |
| 4. P1 features | F9 through F13 | Model access and spend limits managed as code in production |
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
| Cloud Agents API is beta, worker pools use a v0 API, and model access is preview, so schemas may change | Contract monitor, exact version pins, playground-first rollout |
| Upstream changes conflict with fork changes | Additive design, weekly syncs, `FORK.md` |
| Upstream later adds a resource with the same name but a different schema | Coordinate through upstream issues before building; deprecation and migration path |
| "Only admins can change secrets" blocks service account keys (`403 team_admin_required`) | Leave the setting off in the production team, where all humans are admins, and alert on secret events from any actor other than the Terraform service account |
| Admin and Organization keys carry broad scopes | Narrowest scope available; exposed only to the protected apply job; rotated on a schedule |
| Write-only attributes require Terraform 1.11 or later | Enforce `required_version` in consumer stacks; confirm OpenTofu support if used |

## Open questions for Cursor

1. Will environment, secret, worker pool and Admin API resources be added to the official provider, and are contributions accepted?
2. Will the worker pool endpoints move from v0 to v1, and on what compatibility terms?
3. When will model access leave preview, and is its schema expected to change?
4. Can `GET /v1/me` return the key's team ID, so stacks can assert the target team directly?
5. Is read access to team settings (self-hosted requirement, network policy, secrets restriction, follow-ups) planned for the Admin API, so the settings baseline can be checked at plan time?
6. How is a user spend limit cleared through the Admin API?
