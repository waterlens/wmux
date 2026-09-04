#!/bin/sh
set -eu

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_dir"

package_version=$(node -p "require('./package.json').version")
version=${WMUX_VERSION:-$package_version}
commit=${WMUX_COMMIT:-unknown}

if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if [ -z "${WMUX_VERSION:-}" ]; then
    version=$(git describe --tags --always --dirty 2>/dev/null || printf '%s' "$package_version")
  fi
  if [ -z "${WMUX_COMMIT:-}" ]; then
    commit=$(git rev-parse --short=12 HEAD 2>/dev/null || printf '%s' unknown)
  fi
fi

mkdir -p dist
go build -trimpath \
  -ldflags "-X github.com/waterlens/wmux/internal/version.Version=$version -X github.com/waterlens/wmux/internal/version.Commit=$commit" \
  -o dist/wmux ./cmd/wmux
