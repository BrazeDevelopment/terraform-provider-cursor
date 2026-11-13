data "cursor_api_key_info" "current" {
  lifecycle {
    # user_email is null for a team service-account key.
    postcondition {
      condition     = self.user_email == null
      error_message = "api_key must be a team service-account key, not a personal user key."
    }
  }
}
