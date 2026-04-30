#!/usr/bin/env bash
# Build and push ghcr.io/5stackgg/demo-parser from the local
# demo-parser/ context.
#
# Consumed by api/Dockerfile via:
#   FROM ghcr.io/5stackgg/demo-parser:<tag> AS demo-parser
#   COPY --from=demo-parser /usr/local/bin/demo-parser /usr/local/bin/demo-parser
#
# Usage:
#   ./push-latest.sh                       # tag :latest only
#   DEMO_PARSER_REF=v0.1.0 ./push-latest.sh   # also tag :v0.1.0
set -euo pipefail

IMAGE="ghcr.io/5stackgg/demo-parser"
CACHE_REF="${IMAGE}:buildcache"

DEMO_PARSER_REF="${DEMO_PARSER_REF:-latest}"

# Sanitize the ref for use as a docker tag: replace anything outside
# [A-Za-z0-9._-] with `-`. Refs like `feature/foo` would otherwise be
# rejected by the registry.
REF_TAG="$(printf '%s' "$DEMO_PARSER_REF" | tr -c 'A-Za-z0-9._-' '-')"

cd "$(dirname "$0")"

echo "building $IMAGE"
echo "  -> tags: ${IMAGE}:latest ${IMAGE}:${REF_TAG}"

# Build for amd64 only — that matches every node we run k8s on. If
# we ever need arm builds, append `,linux/arm64` here.
docker buildx build \
  --platform linux/amd64 \
  --push \
  --tag "${IMAGE}:latest" \
  --tag "${IMAGE}:${REF_TAG}" \
  --cache-from "type=registry,ref=${CACHE_REF}" \
  --cache-to "type=registry,ref=${CACHE_REF},mode=max" \
  .

echo
echo "done. pin in api with:"
echo "  docker build --build-arg DEMO_PARSER_IMAGE=${IMAGE}:${REF_TAG} ."
