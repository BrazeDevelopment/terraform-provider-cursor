# scope takes the same values as cursor_private_worker_pool ("team", "user"). Omit it to list every pool the key can see.
data "cursor_private_worker_pools" "team" {
  scope = "team"
}

output "idle_team_pools" {
  value = [
    for pool in data.cursor_private_worker_pools.team.pools :
    pool.id if pool.connected_worker_count == 0
  ]
}
