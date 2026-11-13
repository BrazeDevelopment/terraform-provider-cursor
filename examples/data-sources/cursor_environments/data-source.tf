data "cursor_environments" "all" {}

locals {
  # Team environment names are unique, so they can key a map. name is null for unnamed environments.
  team_environment_ids_by_name = {
    for environment in data.cursor_environments.all.environments :
    environment.name => environment.id if environment.owner == "team" && environment.name != null
  }
}
