# Looks up an environment that is managed outside this configuration.
data "cursor_environment" "web" {
  environment_id = var.environment_id
}

output "environment_repos" {
  value = data.cursor_environment.web.repos[*].url
}
