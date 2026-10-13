terraform {
  required_providers {
    cursor = {
      source  = "cursor/cursor"
      version = "~> 0.1"
    }
  }
}

provider "cursor" {
  # Or set the CURSOR_TOKEN environment variable.
  # Optional when api_key, team_api_key, or pool_api_key is set and the stack does not manage automations.
  token = var.cursor_token

  # Cloud Agents API key. A raw key_ or crsr_ token is captured before session exchange and used when api_key is unset.
  # Or set CURSOR_API_KEY.
  api_key = var.cursor_api_key

  # Admin API keys. Or set CURSOR_TEAM_API_KEY and CURSOR_ORGANIZATION_API_KEY.
  team_api_key         = var.cursor_team_api_key
  organization_api_key = var.cursor_organization_api_key

  # Caller-supplied public organization id (org_…). Or set CURSOR_ORGANIZATION_ID.
  organization_id = var.cursor_organization_id

  # Team service-account key for private worker pools. Defaults to api_key. Or set CURSOR_POOL_API_KEY.
  pool_api_key = var.cursor_pool_api_key

  guardrails {
    allowed_automation_scopes = ["team"]
    require_pool_label        = true
    allowed_models            = ["composer-2"]
    allowed_repo_owners       = ["example-org"]
    deny_pr_approval          = true
  }
}
