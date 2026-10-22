#!/bin/sh
#
# Lay out the install tree that the package (and `make install`) ships.
#
#   scripts/stage.sh                 # uses dist/<goos>-<goarch>/ from build.sh
#   BIN=/path/to/binaries scripts/stage.sh
#
# The result mirrors what lands on the host: $STAGE$PREFIX/bin, the rc.d
# script with %%PREFIX%% substituted, the sample config, and the newsyslog
# rule. Nothing here is FreeBSD-specific, so a Linux box can stage a FreeBSD
# tree and hand it to scripts/mkpkg.sh.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

GOOS=${GOOS:-$(go env GOHOSTOS)}
GOARCH=${GOARCH:-$(go env GOHOSTARCH)}
PREFIX=${PREFIX:-/usr/local}
STAGE=${STAGE:-$ROOT/.stage}
BIN=${BIN:-dist/${GOOS}-${GOARCH}}

for f in beastied beastie; do
    [ -x "$BIN/$f" ] || { echo "stage: $BIN/$f missing — run scripts/build.sh first" >&2; exit 1; }
done

rm -rf "$STAGE"
mkdir -p "$STAGE$PREFIX/bin" "$STAGE$PREFIX/etc/rc.d" "$STAGE$PREFIX/etc/newsyslog.conf.d"

cp "$BIN/beastied" "$STAGE$PREFIX/bin/beastied"
cp "$BIN/beastie"  "$STAGE$PREFIX/bin/beastie"
chmod 0755 "$STAGE$PREFIX/bin/beastied" "$STAGE$PREFIX/bin/beastie"

sed "s|%%PREFIX%%|$PREFIX|g" freebsd/beastied.in > "$STAGE$PREFIX/etc/rc.d/beastied"
chmod 0755 "$STAGE$PREFIX/etc/rc.d/beastied"

cp freebsd/beastiemon.conf "$STAGE$PREFIX/etc/beastiemon.conf.sample"
cp freebsd/newsyslog.conf  "$STAGE$PREFIX/etc/newsyslog.conf.d/beastied.conf"
chmod 0644 "$STAGE$PREFIX/etc/beastiemon.conf.sample" \
           "$STAGE$PREFIX/etc/newsyslog.conf.d/beastied.conf"

# /var/log is a base-system directory; the rc.d script creates the log file,
# and the run directory for a Unix-socket listen, at start time.

echo "staged $PREFIX tree in $STAGE"
find "$STAGE" -type f | sed "s|^$STAGE|  |" | sort
