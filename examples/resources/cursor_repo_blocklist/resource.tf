resource "cursor_repo_blocklist" "web" {
  url      = "https://github.com/example-org/web"
  patterns = ["*.env"]
}
