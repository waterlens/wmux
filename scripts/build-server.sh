#!/bin/sh
set -eu

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_dir"

version=${WMUX_VERSION:-}
commit=${WMUX_COMMIT:-}

if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  if [ -z "$version" ]; then
    version=$(git describe --tags --always --dirty 2>/dev/null || printf '')
  fi
  if [ -z "$commit" ]; then
    commit=$(git rev-parse --short=12 HEAD 2>/dev/null || printf '')
  fi
fi

if [ -z "$version" ] && command -v node >/dev/null 2>&1; then
  version=$(node -p "require('./package.json').version" 2>/dev/null || printf '')
fi

# release.sh redirects the output per platform; GOOS/GOARCH pass through from the environment.
output=${WMUX_OUTPUT:-bin/wmux}
mkdir -p "$(dirname -- "$output")"
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X github.com/waterlens/wmux/internal/version.Version=${version:-dev} -X github.com/waterlens/wmux/internal/version.Commit=${commit:-unknown}" \
  -o "$output" ./cmd/wmux
