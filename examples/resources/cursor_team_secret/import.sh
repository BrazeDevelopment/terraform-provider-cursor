# One version: <name>/<secret_version_id>.
terraform import cursor_team_secret.npm NPM_TOKEN/secv_3qKx9mT2bV7nR1cW5yZ8aQ

# A name with a single version: <name>.
terraform import cursor_team_secret.npm NPM_TOKEN

# State has no value_wo_version after import, so the first apply sends value_wo.
