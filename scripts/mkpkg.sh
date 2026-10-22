#!/bin/sh
#
# Build the FreeBSD package from a staged tree (scripts/stage.sh).
#
#   scripts/mkpkg.sh
#   VERSION=0.3.0 ABI=FreeBSD:14:amd64 scripts/mkpkg.sh
#
# Uses pkg(8) when the build host has it — that is the canonical tool, and it
# fills in per-file checksums and the +COMPACT_MANIFEST a repository catalogue
# wants. Everywhere else (a Linux or macOS workstation) it falls back to
# scripts/mkpkg, which writes the same layout with Go's stdlib. pkg(8)
# installs and verifies either one.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

VERSION=${VERSION:-0.2.0}
STAGE=${STAGE:-$ROOT/.stage}
PKGDIR=${PKGDIR:-$ROOT/.pkg}
ABI=${ABI:-FreeBSD:15:amd64}
# PKGFILE, not OUT: scripts/build.sh already uses OUT for its binary
# directory, and a release run exports one environment for both.
PKGFILE=${PKGFILE:-$PKGDIR/beastiemon-$VERSION.pkg}

[ -d "$STAGE" ] || { echo "mkpkg: $STAGE missing — run scripts/stage.sh first" >&2; exit 1; }

mkdir -p "$PKGDIR"

# pkg create stamps the package with the ABI of the host it runs on, so it can
# only cut a package for this machine. Asking it for another architecture
# silently produces an aarch64 payload labelled amd64 — installable on the
# wrong machine and refused by the right one. Cross-arch therefore goes to the
# portable writer, which stamps exactly what was asked for.
native=no
if command -v pkg >/dev/null 2>&1; then
    # ABI has to come out of the environment for this query too: pkg config
    # echoes an ABI set there rather than the host's, which would make every
    # cross-arch request look like a match.
    host_abi=$(env -u ABI pkg config abi 2>/dev/null || true)
    if [ -z "$host_abi" ] || [ "$host_abi" = "$ABI" ]; then
        native=yes
    else
        echo "mkpkg: host is $host_abi but the package targets $ABI"
    fi
fi

if [ "$native" = yes ]; then
    echo "mkpkg: using native pkg(8)"
    sed "s/%%VERSION%%/$VERSION/" freebsd/+MANIFEST > "$STAGE/+MANIFEST"
    cp freebsd/pkg-descr "$STAGE/+DESC"
    # pkg(8) reads ABI from the environment, and finding one set without an
    # OSVERSION makes it guess and warn. It derives the right ABI from the
    # host it runs on, so take ours out of its way.
    env -u ABI pkg create --format txz --manifest "$STAGE/+MANIFEST" \
        --root-dir "$STAGE" --out-dir "$PKGDIR"
    # pkg names the file itself; move it if the caller asked for another name.
    built="$PKGDIR/beastiemon-$VERSION.pkg"
    [ "$built" = "$PKGFILE" ] || mv "$built" "$PKGFILE"
    rm -f "$STAGE/+MANIFEST" "$STAGE/+DESC"
else
    echo "mkpkg: writing the package directly"
    # GOOS/GOARCH are set to the *target* by a cross build; clear them so the
    # helper is compiled for this machine and can actually run here.
    GOOS= GOARCH= go run ./scripts/mkpkg \
        -stage "$STAGE" -manifest freebsd/+MANIFEST \
        -version "$VERSION" -abi "$ABI" -out "$PKGFILE"
fi

echo ""
echo "Package: $PKGFILE"
echo "Install: pkg install $PKGFILE"
