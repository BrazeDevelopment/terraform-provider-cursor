resource "cursor_organization_group_members" "eng" {
  group_id = cursor_organization_group.eng.id
  user_ids = ["user_example"]
}
