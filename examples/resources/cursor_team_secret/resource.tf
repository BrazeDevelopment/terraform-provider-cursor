resource "cursor_team_secret" "npm" {
  name             = "NPM_TOKEN"
  type             = "runtime_secret"
  repos            = ["example-org/web"]
  value_wo         = var.npm_token
  value_wo_version = 1
}
