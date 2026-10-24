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

# Devin's --sandbox mode creates $HOME/.local/share/chisel/sandbox-locks on
# the first exec call. A bind-mount over ~/.local/share (or over
# ~/.local/share/devin in an image whose parents were missing) leaves the
# tree root-owned — Docker auto-creates missing mount parents as root — and
# every sandboxed exec then fails with Permission denied. The image
# pre-creates these dirs; repair here too for mounts that shadow
# .local/share entirely.
mkdir -p "$HOME/.local/share/chisel/sandbox-locks" 2>/dev/null || true
if [ "$(id -u)" = "0" ]; then
  # Started as root (e.g. a `user:` override): hand the tree to the service
  # account. Non-recursive on purpose — a mount over .local/share shouldn't
  # have its contents re-owned, only the dirs devin needs writable.
  chown elyoraa:elyoraa "$HOME/.local" "$HOME/.local/share" \
    "$HOME/.local/share/chisel" \
    "$HOME/.local/share/chisel/sandbox-locks" 2>/dev/null || true
elif [ ! -w "$HOME/.local/share/chisel" ]; then
  echo "entrypoint: warning: $HOME/.local/share/chisel is not writable — devin --sandbox exec calls will fail" >&2
fi

exec /app/elyoraa
