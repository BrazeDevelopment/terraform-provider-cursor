# An ephemeral variable keeps the value out of saved plan files as well as state.
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
