data "cursor_team_members" "current" {}

locals {
  # Removed members stay in the list, so filter them out before using the ids.
  active_member_ids_by_email = {
    for member in data.cursor_team_members.current.members :
    lower(member.email) => member.id if !member.is_removed
  }
}
