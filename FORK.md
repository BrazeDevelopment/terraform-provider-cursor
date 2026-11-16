# Fork ledger

This repository is a fork of [cursor/terraform-provider-cursor](https://github.com/cursor/terraform-provider-cursor). The provider type name stays `cursor`. New behavior is additive. Upstream files are edited only where registration, client fields, or the shared retry policy require it.

## Upstream files

### `internal/provider/provider.go`

Registers the fork resources and data sources. Adds `api_key`, `organization_id`, `pool_api_key`, and the `guardrails` block. `token` is optional when `api_key`, `team_api_key`, `organization_api_key`, or `pool_api_key` is set. A raw `key_` or `crsr_` token is copied into the Cloud Agents API key before that token is exchanged for a session token. `pool_api_key` defaults to `api_key` whenever `api_key` is set. The pools API rejects other key types, and there is no reliable prefix that identifies a service-account key.

Registers each resource through `withPlanGuardrails`, which passes guardrails to resources before a client exists. The registered `cursor_platform_workflow` is `guardedWorkflowResource`, a wrapper around the upstream resource that adds that access; the upstream resource code is unchanged. While a credential is unknown, `Configure` keeps the guardrails so plan checks still run.

Upstream proposal: none yet. Remove this divergence when upstream accepts the same provider configuration.

### `internal/provider/client.go`

Adds `apiKey`, `poolAPIKey`, `organizationID`, `guardrails`, `public`, and `pools` on `apiClient`. The provider data type stays `*apiClient`.

Upstream proposal: none yet. Remove these fields when upstream carries the same client state.

### `internal/provider/retry_client.go`

Exports `HTTPDoer` and `NewRetryClient`. `POST` retries on HTTP 503 only when the response includes `Retry-After`. `POST` responses of 502 and 504 stay unretried. The two-minute wait ceiling is unchanged.

Upstream proposal: none yet. Remove the `POST` exception when upstream retries that case the same way.

### `README.md`

Adds the fork introduction, the private registry installation, and the Fork features section. Upstream sections are unchanged, so a sync conflict here is limited to those additions.

### `.github/workflows/release.yml`

Removed. Upstream's workflow publishes a public GitHub release for the Terraform Registry on every `v*` tag. Fork releases are built from `.goreleaser.yml` and published to the private registry from a private pipeline. Keep the file deleted when an upstream sync changes it.

`resource_platform_workflow.go` and `admin_client.go` are not edited. Guardrails for automations live in `internal/provider/fork_guardrails.go`.

## New packages and files

`internal/publicapi` is the Cloud Agents HTTP client. It uses HTTP Basic auth, the provider retry client, and the user agent `terraform-provider-cursor/<version>+braze`. Request and response types are written to match the pinned spec `openapi/cloud-agents-openapi.yaml` (sourced from `https://cursor.com/docs-static/cloud-agents-openapi.yaml`). They are not generated. Admin and Organization calls live in `internal/provider/fork_admin.go`.

Resources and data sources added by the fork:

- `cursor_environment`
- `cursor_environment_secret`, `cursor_team_secret`
- `cursor_api_key_info`, `cursor_team_members`
- `cursor_environments`, `cursor_environment_active_build`, and the `cursor_environment` data source
- `cursor_models`
- `cursor_private_worker_pool`, `cursor_private_worker_pools`, `cursor_private_workers_summary`
- `cursor_model_access_configuration`, `cursor_model_access_provider`, `cursor_model_access_model`
- `cursor_user_spend_limit`
- `cursor_team_directory_group`, `cursor_team_directory_group_members`
- `cursor_organization_group`, `cursor_organization_group_members`
- `cursor_repo_blocklist`
- provider `guardrails` for automation scope, pool label, model, repository owner, and pull-request approval

Secret values use write-only `value_wo` and are not stored in state or plan. The provider schema that contains them requires Terraform or OpenTofu 1.11 or later.

Upstream proposal: none yet. Remove a resource when upstream ships the same type name and a compatible schema.

## Upstream sync

Upstream `main` is merged with a merge commit, never rebased. The last merged upstream commit is `eaea0f3` (Add trust_app_declared_ips to cursor_origin_inbound_ip_allowlist). Check for new commits with `git fetch upstream main && git log --oneline origin/main..upstream/main`.
