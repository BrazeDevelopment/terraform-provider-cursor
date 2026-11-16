# Terraform Provider for Cursor Automations

Manage [Cursor Automations](https://cursor.com) with Terraform or OpenTofu, and read [Origin](https://cursor.com/docs/origin) repositories from the public Origin API.

This provider exposes Cursor Platform workflows as Terraform resources. It talks to the Cursor Automations API over Connect RPC. Origin repositories are a data source: the public Origin API can create and read repositories, but it has no update or delete operation.

Full documentation for the provider, resources, and data sources lives in [`docs/`](./docs) and on the Terraform Registry.

This repository is Braze's fork of [cursor/terraform-provider-cursor](https://github.com/cursor/terraform-provider-cursor). It adds Cloud Agent environments, secrets, private worker pools, model access, spend controls, groups, a repository blocklist, and plan-time guardrails; see [Fork features](#fork-features). [`FORK.md`](./FORK.md) records every upstream file the fork edits.

## Installation

The fork is published to Braze's private registry, not the public Terraform Registry. Type names match upstream, so moving an existing stack only changes the provider address.

```hcl
terraform {
  required_version = ">= 1.11.0"

  required_providers {
    cursor = {
      source  = "tf.braze.com/braze/cursor"
      version = "1.0.0"
    }
  }
}
```

`tf.braze.com` does not resolve in DNS. Map it to Artifactory in `~/.terraformrc` (or `~/.tofurc`), with Zscaler connected:

```hcl
host "tf.braze.com" {
  services = {
    "modules.v1"   = "https://arti.k8s.core-001.cp-use-1.braze.com/artifactory/api/terraform/terraform/v1/modules/"
    "providers.v1" = "https://arti.k8s.core-001.cp-use-1.braze.com/artifactory/api/terraform/terraform/providers/"
  }
}
```

Move existing state from the upstream provider with `terraform state replace-provider registry.terraform.io/cursor/cursor tf.braze.com/braze/cursor`. Pin an exact version and commit `.terraform.lock.hcl`. Terraform or OpenTofu 1.11 or later is required because secret values use write-only attributes.

## Provider Configuration

Use explicit configuration or environment variables:

- `CURSOR_TOKEN` - Cursor API token for Automations and Origin. Raw `key_` and `crsr_` API keys are exchanged for a session token automatically. Optional when `api_key`, `team_api_key`, `organization_api_key`, or `pool_api_key` is set and the stack does not manage automations.
- `CURSOR_ENDPOINT` - Cursor Automations API base URL. Optional, defaults to `https://api2.cursor.sh`.
- `CURSOR_API_KEY` - Raw Cloud Agents API key. Optional. Defaults to `CURSOR_TOKEN` when that value is still a raw `key_` or `crsr_` key, captured before session exchange.
- `CURSOR_TEAM_API_KEY` - Team Admin API key. Used for Origin grants keyed by `user_email` and for Admin API resources.
- `CURSOR_ORGANIZATION_API_KEY` - Organization Admin API key. Used for Origin grants keyed by `group_name` and for Organization API resources.
- `CURSOR_ORGANIZATION_ID` - Public organization id (`org_…`). Required by Organization API resources.
- `CURSOR_POOL_API_KEY` - Team service-account key for private worker pools. Optional. Defaults to the Cloud Agents API key.

```hcl
provider "cursor" {
  token = var.cursor_token
}
```

## Example

```hcl
resource "cursor_platform_workflow" "example_review" {
  name    = "Example review automation"
  scope   = "team"
  enabled = true

  prompt = file("prompt.md")

  trigger = [
    {
      git_pull_request = {
        repos            = ["example-org/example-repo"]
        pr_action        = "opened"
        ignore_draft_prs = true
      }
    }
  ]

  action = [
    {
      pr_comment = {
        allow_inline_comments = true
      }
    }
  ]
}
```

Supported triggers: `git_pull_request`, `git_push`, `git_ci_completed`, `cron`, `slack`, `linear`, `webhook`, `microsoft_teams`, and `microsoft_teams_channel_created`.

Supported actions: `pr_comment`, `git_pr`, `request_reviewers`, `mcp`, `slack`, `read_slack`, `microsoft_teams`, and `read_microsoft_teams`.

See [`examples/`](./examples) for more, including the data source and import syntax. The resource and data source docs in [`docs/`](./docs) describe every trigger and action type.

## Origin repositories

Read an existing Origin repository with the same provider token. Look up by owner and name, or by the repository's stable ID:

```hcl
data "cursor_origin_repo" "rocket" {
  owner = "acme"
  name  = "rocket"
}
```

`clone_url`, `default_branch`, `visibility`, and mirror metadata come from `GET /v1/origin/repos/{owner}/{name}` on the public Origin API (`https://api.cursor.com/v1/origin`). A raw `key_` or `crsr_` API key is exchanged for a session token first, the same way Automations calls are authenticated.

Manage a repository ruleset with the same token. Each `rule` block sets exactly one typed block; the block name is the Origin rule type and its snake_case arguments map to the camelCase parameters Origin stores. Update replaces the ruleset, including its rules and bypass actors. `deletion_protection` defaults to true: Terraform will not delete the ruleset, or replace it because `owner` or `repo` changed, until that is set to false and applied. A ruleset with no rules, no included refs, or `enforcement = "disabled"` is rejected unless `allow_unenforced` is true. `origin_role` `repository_write` bypasses every principal with write access and requires `allow_broad_bypass`.

```hcl
resource "cursor_origin_repo_ruleset" "main" {
  owner       = "acme"
  repo        = "rocket"
  name        = "require-review"
  enforcement = "active"
  kind        = "merge_branch"

  included_ref_names = ["refs/heads/main"]

  rule {
    pull_request {
      required_approving_review_count = 1
    }
  }

  rule {
    require_status_checks {
      required_check {
        actor_kind = "app"
        actor_id   = "app_01k2ja2000e0080000000000c1"
        group_key  = "ci"
      }
    }
  }
}
```

Merge rules (`pull_request`, `require_status_checks`, `require_branch_up_to_date`) apply to `kind = "merge_branch"`. Push rules apply to the `push_branch`, `push_tag`, and `push_repository` kinds: `deletion`, `non_fast_forward`, `block_direct_updates`, `block_merges`, and `required_linear_history` take no arguments, for example `rule { deletion {} }`, and `ref_name_pattern { pattern = "refs/tags/v*" }` restricts ref names (set `negate = true` to reject matches instead).

Manage direct access grants with `cursor_origin_repo_grant` (repository, `permission` = `read`, `write`, or `admin`) and `cursor_origin_owner_grant` (namespace, `permission` = `read`, `contributor`, `write`, or `admin`). Name the principal with exactly one of `user_email`, `group_name`, `user = { id = "user_..." }`, `group = { id = "grp_..." }`, or `team_group = { kind = "members" | "admins" }`. `user_email` is resolved through the Team Admin API (`GET https://api.cursor.com/teams/members`, provider `team_api_key` / `CURSOR_TEAM_API_KEY`) and `group_name` through the Organization Admin API (`GET https://api.cursor.com/organizations/groups?name=<group_name>`, exact match, using the group's `publicId`, provider `organization_api_key` / `CURSOR_ORGANIZATION_API_KEY`); both keys are sent as HTTP Basic auth with an empty password. The resolved ID is stored in `user` or `group`, and a grant is identified by that ID and the resource rather than a server ID: create and update both upsert over `POST .../grants`, a changed resolution replaces the grant, and refresh drops a grant from state when the principal no longer appears in the list. Import with `owner/repo:user_email:alice@acme.com`, `owner:group_name:Engineering`, `owner/repo:user:user_01...`, or `owner:team_group:admins`. `cursor_origin_repo_grants` and `cursor_origin_owner_grants` list the direct grants on a resource.

```hcl
resource "cursor_origin_repo_grant" "alice" {
  owner      = "acme"
  repo       = "rocket"
  permission = "write"
  user_email = "alice@acme.com"
}

resource "cursor_origin_repo_grant" "eng" {
  owner      = "acme"
  repo       = "rocket"
  permission = "read"
  group_name = "Engineering"
}
```

## Fork features

Each resource supports create, refresh, in-place update, destroy, and `terraform import`; the import format is on its docs page and in `examples/resources/<type>/import.sh`. Create refuses to take over an object that already exists and names the import id to use instead.

| Area | Resources | Data sources | Credential |
|---|---|---|---|
| Cloud Agent environments | `cursor_environment` | `cursor_environment`, `cursor_environments`, `cursor_environment_active_build` | `api_key` |
| Secrets | `cursor_environment_secret`, `cursor_team_secret` | | `api_key` |
| Private worker pools | `cursor_private_worker_pool` | `cursor_private_worker_pools`, `cursor_private_workers_summary` | `pool_api_key` |
| Model access | `cursor_model_access_configuration`, `cursor_model_access_provider`, `cursor_model_access_model` | `cursor_models` | `team_api_key` (model reads use `api_key`) |
| Spend and repositories | `cursor_user_spend_limit`, `cursor_repo_blocklist` | | `team_api_key` |
| Team directory groups | `cursor_team_directory_group`, `cursor_team_directory_group_members` | `cursor_team_members` | `team_api_key` |
| Organization groups | `cursor_organization_group`, `cursor_organization_group_members` | | `organization_api_key` and `organization_id` |
| Identity | | `cursor_api_key_info` | `api_key` |

### Environments and secrets

```hcl
resource "cursor_environment" "web" {
  owner = "team"
  name  = "web"
  repos = [{ url = "https://github.com/example-org/web" }]
  environment_json = jsonencode({
    install = "pnpm install"
  })
}

variable "npm_token" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "cursor_team_secret" "npm" {
  name             = "NPM_TOKEN"
  type             = "runtime_secret"
  repos            = ["example-org/web"]
  value_wo         = var.npm_token
  value_wo_version = 1
}
```

Secret values are write-only: they are sent on create and update and never stored in state or plan files. Increase `value_wo_version` to rotate a value. Each secret resource manages one version of a name, and `repos` must list at least one repository, because an empty list grants every repository. Build Secrets cannot be created or changed through the API; one changed to a Build Secret outside Terraform can still be destroyed.

Self-hosted pool workers use an environment's repository list only, and they receive team secrets only when Secret sync is on and the worker runs with `--sync-dashboard-secrets`.

### Private worker pools

```hcl
resource "cursor_private_worker_pool" "gpu" {
  scope                        = "team"
  pool_name                    = "gpu"
  worker_ready_timeout_seconds = 0
}
```

Set `repo_owner` and `repo_name` for a repo-backed pool. The pools API accepts only a team service-account key.

### Model access, spend, and groups

Apply `cursor_model_access_configuration` before any provider or model resource: those routes return 409 until a custom policy exists. Destroying a provider or model disables it. Destroying the configuration is blocked while `deletion_protection` is true.

`cursor_user_spend_limit` sets one member's monthly limit by email. Group resources take the `team_group_` or `g_` id, not the `grp_` public id. A members resource owns the group's entire member list, and ids the API ignores fail the apply.

### Guardrails

```hcl
provider "cursor" {
  guardrails {
    allowed_automation_scopes = ["team"]
    require_pool_label        = true
    allowed_models            = ["composer-2"]
    allowed_repo_owners       = ["example-org"]
    deny_pr_approval          = true
  }
}
```

Guardrails fail the plan when an automation, environment, or repo-backed pool violates them. They run even when provider credentials are known only after apply, and they never block a destroy. Guardrail values must be known at plan time. They constrain only this provider, not launches from the dashboard or API, and `terraform import` does not run them.

## Development

```bash
make build   # go build
make test    # go test ./...
make docs    # regenerate docs/ with tfplugindocs
```

Regenerate `docs/` with `make docs` after changing any schema `Description` or the files under `examples/`.

## License

[Apache-2.0](./LICENSE). "Cursor" is a trademark of Anysphere, Inc.
