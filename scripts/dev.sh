#!/bin/sh
# Rebuild + restart demo-parser on every .go save. Works locally and
# inside a codepier-synced pod (alpine ships /bin/sh, not bash).
set -eu

cd "$(dirname "$0")/.."

# Pinned: air@latest currently requires Go 1.25; this repo is on 1.24.
AIR_VERSION="v1.61.7"

if ! command -v air >/dev/null 2>&1; then
  echo "installing air ${AIR_VERSION}..."
  go install github.com/air-verse/air@${AIR_VERSION}
fi

GOBIN="$(go env GOPATH)/bin"
export PATH="$GOBIN:$PATH"

exec air -c .air.toml
