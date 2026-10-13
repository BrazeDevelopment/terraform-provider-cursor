resource "cursor_model_access_model" "opus" {
  provider_id = "anthropic"
  model_id    = "claude-opus-4-6"
  enabled     = true

  depends_on = [cursor_model_access_configuration.team]
}
