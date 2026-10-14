resource "cursor_environment" "web" {
  owner = "team"
  name  = "web"
  repos = [
    { url = "https://github.com/example-org/web" },
  ]
  environment_json = jsonencode({
    install = "pnpm install"
    start   = "pnpm dev"
  })
}
