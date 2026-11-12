# Any-repo pool: <scope>/<pool_name>, where scope is team or user.
terraform import cursor_private_worker_pool.gpu team/gpu

# Repo-backed pool: <scope>/<pool_name>/<repo_owner>/<repo_name>.
terraform import cursor_private_worker_pool.api team/gpu/acme/api

# Inside a name, write / as %2F and % as %25.
terraform import cursor_private_worker_pool.nested team/ml%2Fgpu
