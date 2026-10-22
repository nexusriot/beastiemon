#!/bin/sh
#
# Build every shipped architecture and package each one.
#
#   scripts/release.sh                      # freebsd/amd64 + freebsd/arm64
#   ARCHES="amd64" VERSION=0.5.0 scripts/release.sh
#
# Leaves binaries under dist/<goos>-<goarch>/ and packages in .pkg/, with a
# SHA256 manifest beside them.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

VERSION=${VERSION:-0.4.0}
GOOS=${GOOS:-freebsd}
ARCHES=${ARCHES:-amd64 arm64}
ABI_VERSION=${ABI_VERSION:-15}
PKGDIR=${PKGDIR:-$ROOT/.pkg}

mkdir -p "$PKGDIR"

for arch in $ARCHES; do
    # pkg spells aarch64 where Go says arm64; the ABI string has to match the
    # target or pkg(8) refuses the package as built for another machine.
    case $arch in
        amd64) pkgarch=amd64 ;;
        arm64) pkgarch=aarch64 ;;
        *)     pkgarch=$arch ;;
    esac

    echo "=== $GOOS/$arch ==="
    GOOS="$GOOS" GOARCH="$arch" VERSION="$VERSION" sh scripts/build.sh
    GOOS="$GOOS" GOARCH="$arch" sh scripts/stage.sh >/dev/null
    GOOS="$GOOS" GOARCH="$arch" VERSION="$VERSION" \
        ABI="FreeBSD:$ABI_VERSION:$pkgarch" \
        PKGFILE="$PKGDIR/beastiemon-$VERSION-$pkgarch.pkg" \
        sh scripts/mkpkg.sh | tail -3
    echo ""
done

echo "=== checksums ==="
( cd "$PKGDIR" && \
  if command -v sha256sum >/dev/null 2>&1; then
      sha256sum ./*.pkg > SHA256SUMS
  else
      sha256 -r ./*.pkg > SHA256SUMS
  fi
  cat SHA256SUMS )
