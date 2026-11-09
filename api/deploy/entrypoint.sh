#!/bin/sh
set -e

# Load an env file if provided or if a .env exists in the working directory.
ENV_FILE="${ENV_FILE:-/app/.env}"
if [ -f "$ENV_FILE" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

exec /app/elyoraa
