data "cursor_environment_active_build" "web" {
  environment_id = cursor_environment.web.id
}

output "web_environment_build" {
  # type is "universal_image" when no build is active; build_id and status are then null.
  value = {
    type     = data.cursor_environment_active_build.web.type
    build_id = data.cursor_environment_active_build.web.build_id
    status   = data.cursor_environment_active_build.web.status
  }
}
