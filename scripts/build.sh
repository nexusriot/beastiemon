#!/bin/sh
#
# Build the beastiemon binaries for one target.
#
#   scripts/build.sh                     # this machine
#   GOOS=freebsd GOARCH=arm64 scripts/build.sh
#   OUT=/tmp/b scripts/build.sh
#
# Unset GOOS/GOARCH mean "build for this machine"; the Makefile exports
# freebsd/amd64, which is what a release targets.
#
# Binaries land in $OUT (default dist/<goos>-<goarch>/). CGO is off so both
# binaries stay statically linked with no shared-library dependencies — the
# single-static-binary story the README promises, including SQLite history
# via the pure-Go driver.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

VERSION=${VERSION:-0.4.0}
GOOS=${GOOS:-$(go env GOHOSTOS)}
GOARCH=${GOARCH:-$(go env GOHOSTARCH)}
OUT=${OUT:-dist/${GOOS}-${GOARCH}}

mkdir -p "$OUT"

# -trimpath keeps build paths out of the binary so two machines produce the
# same bytes; -s -w drop the symbol table and DWARF.
for cmd in beastied beastie; do
    GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
        -o "$OUT/$cmd" "./cmd/$cmd"
done

echo "built $GOOS/$GOARCH $VERSION -> $OUT"
ls -l "$OUT/beastied" "$OUT/beastie" | awk '{printf "  %-10s %s bytes\n", $NF, $5}'
