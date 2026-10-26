#!/usr/bin/env bash
# Cross-compiles chatvault for every release platform and packages it into dist/,
# named per the appkit release conventions
# (https://github.com/soundadam/appkit#发布产物):
#
#   chatvault-<ver>-macos-<arch>.tar.gz
#
# each holding chatvault-<ver>-macos-<arch>/chatvault with README.md, LICENSE and
# THIRD_PARTY_LICENSES. Only macOS is built: chatvault reads the macOS WeChat
# client's data. The version is injected as main.version; the release workflow
# passes the tag without its leading "v". It needs GNU tar and touch, so the
# release runs it on Linux.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ "$#" -ne 1 ] || [ -z "$1" ]; then
    echo "usage: $0 VERSION" >&2
    exit 1
fi
version=${1#v}
extras=(README.md LICENSE)
# Archive timestamps come from the commit, so the same commit and version
# package to the same bytes.
mtime=$(git log -1 --format=%cI)

# THIRD_PARTY_LICENSES for one platform: the license texts of the Go runtime
# and standard library, then of every module that platform's build links.
third_party_licenses() {
    local goos=$1 goarch=$2 module dir f files
    printf 'chatvault links the following third-party code. Their licenses follow.\n'
    printf '\n==== Go standard library and runtime (%s)\n\n' "$(go env GOVERSION)"
    cat "$(go env GOROOT)/LICENSE"
    GOOS=$goos GOARCH=$goarch go list -deps \
        -f '{{with .Module}}{{if not .Main}}{{.Path}}@{{.Version}}{{"\t"}}{{.Dir}}{{end}}{{end}}' ./cmd/chatvault |
        LC_ALL=C sort -u |
        while IFS=$'\t' read -r module dir; do
            files=()
            for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE* "$dir"/PATENTS*; do
                [ -f "$f" ] && files+=("$f")
            done
            if [ "${#files[@]}" -eq 0 ]; then
                echo "no license file in $module ($dir)" >&2
                return 1
            fi
            for f in "${files[@]}"; do
                printf '\n==== %s (%s)\n\n' "$module" "$(basename "$f")"
                cat "$f"
            done
        done
}

rm -rf dist
mkdir -p dist

for platform in darwin/amd64 darwin/arm64; do
    goos=${platform%/*}
    goarch=${platform#*/}
    case $goos in darwin) os=macos ;; *) os=$goos ;; esac
    case $goarch in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; esac
    name="chatvault-$version-$os-$arch"
    mkdir -p "dist/$name"
    CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath \
        -ldflags "-s -w -buildid= -X main.version=$version" \
        -o "dist/$name/chatvault" ./cmd/chatvault
    cp "${extras[@]}" "dist/$name/"
    third_party_licenses "$goos" "$goarch" >"dist/$name/THIRD_PARTY_LICENSES"
    touch -d "$mtime" "dist/$name" "dist/$name"/*

    tar -C dist --sort=name --owner=0 --group=0 --numeric-owner \
        --mtime="$mtime" -cf - "$name" | gzip -n >"dist/$name.tar.gz"
    rm -rf "dist/${name:?}"
done

ls -l dist
