#!/bin/sh
# GIT_ASKPASS helper for elyoraa.
# The installation token is written to $ELYORAA_GIT_TOKEN_FILE before each git call.
#
# Credentials are only answered for prompts on $ELYORAA_GIT_HOST. Agent-driven
# git runs can name arbitrary remotes; without the host check a prompt like
# "Password for 'https://attacker.example/repo':" would hand over the
# installation token. The host is parsed out of the quoted URL rather than
# substring-matched — a crafted URL must not smuggle the allowed host into a
# path or userinfo segment (e.g. https://evil/x?://github.com/).
prompt=$1

url=${prompt#*\'}     # text after the first quote
url=${url%%\'*}       # ...up to the next quote
rest=${url#*://}      # strip "scheme://" (unchanged if none)
authority=${rest%%/*} # host[:port], possibly with userinfo
host=${authority##*@} # userinfo may itself contain @ — take the last segment
host=${host%%:*}      # strip :port

if [ -z "$ELYORAA_GIT_HOST" ] || [ "$host" != "$ELYORAA_GIT_HOST" ]; then
  echo "git-askpass: refusing credentials for host '${host:-unknown}' (allowed: '${ELYORAA_GIT_HOST:-unset}')" >&2
  exit 1
fi

case "$prompt" in
  *Username*) echo "x-access-token" ;;
  *Password*) cat "$ELYORAA_GIT_TOKEN_FILE" ;;
esac
