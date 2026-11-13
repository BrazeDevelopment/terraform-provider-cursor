data "cursor_models" "catalog" {}

output "model_ids" {
  value = data.cursor_models.catalog.models[*].id
}
