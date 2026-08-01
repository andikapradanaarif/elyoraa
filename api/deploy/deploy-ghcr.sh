#!/bin/sh
# Build and push the elyoraa image to GitHub Container Registry (ghcr.io).
#
# Usage:
#   ./deploy/deploy-ghcr.sh
#   ./deploy/deploy-ghcr.sh --no-cache
#
# Run from the api/ directory:
#   cd api && ./deploy/deploy-ghcr.sh
#
# Environment:
#   GITHUB_USER   defaults to andikapradanaarif; override for a different GHCR org.
#   GHCR_TOKEN    optional; if unset you will be prompted for a PAT with write:packages.

set -e

# --- CONFIGURATION ---
IMAGE_NAME="elyoraa"
TAG="${TAG:-latest}"
PLATFORM="${PLATFORM:-linux/amd64}"
GITHUB_USER="${GITHUB_USER:-andikapradanaarif}"

GHCR_REPO="ghcr.io/${GITHUB_USER}/${IMAGE_NAME}"

# --- Resolve script location and cd to api/ ---
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
API_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$API_ROOT"

# --- Parse args ---
BUILD_FLAGS=""
for arg in "$@"; do
  case "$arg" in
    --no-cache) BUILD_FLAGS="--no-cache" ;;
    *) echo "Unknown option: $arg"; exit 1 ;;
  esac
done

# --- BUILD DOCKER IMAGE ---
echo "Building Docker image for $PLATFORM (context: $API_ROOT)..."
docker build --platform "$PLATFORM" -f deploy/Dockerfile $BUILD_FLAGS -t "$IMAGE_NAME" . \
  || { echo "Build failed"; exit 1; }

# --- TAG FOR GHCR ---
echo "Tagging image for GHCR..."
docker tag "$IMAGE_NAME:latest" "$GHCR_REPO:$TAG" \
  || { echo "Tagging failed"; exit 1; }

# --- AUTHENTICATE WITH GHCR ---
echo "Logging in to GitHub Container Registry..."

if [ -z "${GHCR_TOKEN:-}" ]; then
  printf 'Enter your GitHub Personal Access Token (with write:packages scope): '
  read -r GHCR_TOKEN
fi

if ! echo "$GHCR_TOKEN" | docker login ghcr.io -u "$GITHUB_USER" --password-stdin; then
  echo "Login to GHCR failed"
  exit 1
fi

# --- PUSH IMAGE TO GHCR ---
echo "Pushing image to GHCR..."
docker push "$GHCR_REPO:$TAG" || { echo "Push failed"; exit 1; }

echo "Success: image pushed to $GHCR_REPO:$TAG"

echo ""
echo "To deploy on your server, update your .env and run:"
echo "  cd deploy"
echo "  docker compose pull"
echo "  docker compose up -d"
