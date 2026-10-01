#!/usr/bin/env bash
set -euo pipefail

# Build the two supported release archives from the exact release tag at HEAD.
# Publishing remains a separate, approval-gated action described in README.md.

output_dir=${1:-dist}
version=$(git describe --exact-match --tags HEAD)
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'HEAD must carry a SemVer release tag (got %s)\n' "$version" >&2
  exit 2
fi

if [[ "$(git cat-file -t "refs/tags/$version")" != "tag" ]]; then
  printf 'release tag %s must be annotated\n' "$version" >&2
  exit 2
fi

if [[ -n "$(git status --porcelain)" ]]; then
  printf 'release packaging requires a clean worktree\n' >&2
  exit 2
fi

if [[ ! -f LICENSE ]]; then
  printf 'release packaging requires the selected LICENSE file\n' >&2
  exit 2
fi

toolchain=$(awk '$1 == "toolchain" { print $2 }' go.mod)
if [[ ! "$toolchain" =~ ^go[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'go.mod must declare an exact toolchain\n' >&2
  exit 2
fi

checksum="$output_dir/SHA256SUMS"
if [[ -e "$checksum" || -L "$checksum" ]]; then
  printf 'refusing to replace existing checksum artifact %s\n' "$checksum" >&2
  exit 2
fi

mkdir -p "$output_dir"
temporary_dir=$(mktemp -d "$output_dir/.codex-model-router-package.XXXXXX")
trap 'rm -rf "$temporary_dir"' EXIT

module_path='github.com/zydtiger/codex-model-router/internal/routing.Version'
for target in darwin/arm64 linux/amd64; do
  os=${target%/*}
  arch=${target#*/}
  stem="codex-model-router_${version#v}_${os}_${arch}"
  archive="${stem}.tar.gz"
  if [[ -e "$output_dir/$archive" || -L "$output_dir/$archive" ]]; then
    printf 'refusing to replace existing artifact %s\n' "$output_dir/$archive" >&2
    exit 2
  fi
  stage="$temporary_dir/$stem"
  mkdir "$stage"
  GOTOOLCHAIN="$toolchain" GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X ${module_path}=${version}" \
    -o "$stage/codex-model-router" ./cmd/codex-model-router
  cp README.md CHANGELOG.md LICENSE "$stage/"
  tar -C "$temporary_dir" -czf "$temporary_dir/$archive" "$stem"
done

(
  cd "$temporary_dir"
  if command -v sha256sum >/dev/null; then
    sha256sum ./*.tar.gz > SHA256SUMS
  else
    shasum -a 256 ./*.tar.gz > SHA256SUMS
  fi
)

mv "$temporary_dir"/*.tar.gz "$temporary_dir/SHA256SUMS" "$output_dir/"
printf 'wrote release artifacts to %s for %s\n' "$output_dir" "$version"
