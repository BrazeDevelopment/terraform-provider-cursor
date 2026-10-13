resource "cursor_model_access_provider" "anthropic" {
  provider_id = "anthropic"
  enabled     = true

  depends_on = [cursor_model_access_configuration.team]
}
