resource "cursor_team_directory_group_members" "eng" {
  group_id = cursor_team_directory_group.eng.id
  user_ids = ["user_example"]
}
