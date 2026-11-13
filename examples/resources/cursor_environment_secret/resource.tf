# An ephemeral variable keeps the value out of saved plan files as well as state.
variable "npm_token" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "cursor_environment_secret" "npm" {
  environment_id   = cursor_environment.web.id
  name             = "NPM_TOKEN"
  type             = "runtime_secret"
  repos            = ["example-org/web"]
  value_wo         = var.npm_token
  value_wo_version = 1
}
