# One version: <environment_id>/<name>/<secret_version_id>.
terraform import cursor_environment_secret.npm 8f14e45f-ceea-4e6b-9c3a-1d2e3f4a5b6c/NPM_TOKEN/secv_3qKx9mT2bV7nR1cW5yZ8aQ

# A name with a single version: <environment_id>/<name>.
terraform import cursor_environment_secret.npm 8f14e45f-ceea-4e6b-9c3a-1d2e3f4a5b6c/NPM_TOKEN

# State has no value_wo_version after import, so the first apply sends value_wo.
