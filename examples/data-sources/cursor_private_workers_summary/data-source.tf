data "cursor_private_workers_summary" "current" {}

output "team_workers_connected" {
  # Null when the key has no team summary.
  value = data.cursor_private_workers_summary.current.team_total_connected
}
