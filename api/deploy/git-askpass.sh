#!/bin/sh
# GIT_ASKPASS helper for elyoraa.
# The installation token is written to $ELYORAA_GIT_TOKEN_FILE before each git call.
case "$1" in
  *Username*) echo "x-access-token" ;;
  *Password*) cat "$ELYORAA_GIT_TOKEN_FILE" ;;
esac
