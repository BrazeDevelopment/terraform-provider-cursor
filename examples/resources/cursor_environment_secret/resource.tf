resource "cursor_environment_secret" "npm" {
  environment_id   = cursor_environment.web.id
  name             = "NPM_TOKEN"
  type             = "runtime_secret"
  repos            = ["example-org/web"]
  value_wo         = var.npm_token
  value_wo_version = 1
}
