resource "cursor_private_worker_pool" "gpu" {
  scope                        = "team"
  pool_name                    = "gpu"
  worker_ready_timeout_seconds = 0
}
