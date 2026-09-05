#!/bin/sh
# Builds release archives for every supported platform into release/.
#
#   scripts/release.sh [--version vX.Y.Z] [--targets "linux/amd64 darwin/arm64"] [--publish]
#
# Each archive is wmux_<version>_<os>_<arch>.tar.gz and contains the wmux
# binary, README.md and the deploy/ examples; release/SHA256SUMS covers all
# archives. The embedded web UI is rebuilt when pnpm is available, otherwise the
# committed internal/webui/dist is used. --publish uploads everything to the
# GitHub release for <version> through the gh CLI (the tag must already exist).
set -eu

project_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$project_dir"

usage() {
  sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'
}

version=${WMUX_VERSION:-}
targets=${WMUX_RELEASE_TARGETS:-"linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"}
publish=0
while [ $# -gt 0 ]; do
  case $1 in
    --version)
      version=$2
      shift 2
      ;;
    --version=*)
      version=${1#*=}
      shift
      ;;
    --targets)
      targets=$2
      shift 2
      ;;
    --targets=*)
      targets=${1#*=}
      shift
      ;;
    --publish)
      publish=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "release.sh: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [ -z "$version" ]; then
  # A tagged commit releases under its tag; anything else gets a describe string.
  version=$(git describe --tags --exact-match 2>/dev/null || git describe --tags --always --dirty)
fi
case $version in
  *[!A-Za-z0-9._+-]*)
    echo "release.sh: version '$version' contains characters that are unsafe in file names" >&2
    exit 2
    ;;
esac
commit=$(git rev-parse --short=12 HEAD)

if command -v pnpm >/dev/null 2>&1; then
  pnpm install --frozen-lockfile
  pnpm build:web
else
  echo "release.sh: pnpm not found, using the committed internal/webui/dist" >&2
fi

out=release
rm -rf "$out"
mkdir -p "$out"

for target in $targets; do
  os=${target%/*}
  arch=${target#*/}
  name="wmux_${version}_${os}_${arch}"
  stage="$out/$name"
  echo "building $name"
  mkdir -p "$stage/deploy"
  WMUX_VERSION=$version WMUX_COMMIT=$commit WMUX_OUTPUT="$stage/wmux" GOOS=$os GOARCH=$arch \
    sh scripts/build-server.sh
  cp README.md "$stage/"
  cp deploy/wmux.service.example deploy/wmux.env.example deploy/Caddyfile.example "$stage/deploy/"
  tar -czf "$out/$name.tar.gz" -C "$out" "$name"
  rm -rf "$stage"
done

# Checksums are listed relative to release/ so `sha256sum -c SHA256SUMS` works in place.
(
  cd "$out"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum ./*.tar.gz >SHA256SUMS
  else
    shasum -a 256 ./*.tar.gz >SHA256SUMS
  fi
  sed -i.bak 's|\./||' SHA256SUMS && rm -f SHA256SUMS.bak
)

echo
echo "release $version ($commit):"
ls -l "$out"

if [ "$publish" = 1 ]; then
  if ! command -v gh >/dev/null 2>&1; then
    echo "release.sh: --publish needs the gh CLI" >&2
    exit 1
  fi
  gh release create "$version" "$out"/*.tar.gz "$out/SHA256SUMS" --title "wmux $version" --generate-notes
fi
