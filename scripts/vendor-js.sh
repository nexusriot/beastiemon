#!/bin/sh
#
# Fetch uPlot into web/vendor/ and point the embedded dashboard at it, so the
# shipped binary needs no CDN at runtime.
#
# The committed index.html and assets.go deliberately stay on their CDN
# defaults (see .gitignore), so a plain `go build` works without this step;
# running it rewrites both in place. It is idempotent: the $ anchor on the
# go:embed line stops a second run appending " vendor" twice.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

UPLOT_VER=${UPLOT_VER:-1.6.27}
UPLOT_URL=${UPLOT_URL:-https://unpkg.com/uplot@$UPLOT_VER/dist}

# sed -i is spelled differently on GNU and BSD, and this script has to run on
# both a FreeBSD host and a Linux build box. A temp file sidesteps the whole
# argument.
inplace() {
    file=$1
    shift
    tmp="$file.vendor-js.$$"
    sed "$@" "$file" > "$tmp" && mv "$tmp" "$file"
}

fetch_to() {
    url=$1
    dest=$2
    if command -v fetch >/dev/null 2>&1; then
        fetch -q -o "$dest" "$url"
    elif command -v curl >/dev/null 2>&1; then
        curl -fsSL -o "$dest" "$url"
    else
        echo "vendor-js: need fetch(1) or curl(1)" >&2
        exit 1
    fi
    [ -s "$dest" ] || { echo "vendor-js: $dest came back empty" >&2; exit 1; }
}

mkdir -p web/vendor
echo "vendor-js: fetching uPlot $UPLOT_VER"
fetch_to "$UPLOT_URL/uPlot.iife.min.js" web/vendor/uplot.iife.min.js
fetch_to "$UPLOT_URL/uPlot.min.css"     web/vendor/uplot.min.css

inplace web/index.html \
    -e 's|https://unpkg.com/uplot@[^/]*/dist/uPlot.iife.min.js|vendor/uplot.iife.min.js|g' \
    -e 's|https://unpkg.com/uplot@[^/]*/dist/uPlot.min.css|vendor/uplot.min.css|g'

# The embed directive has to grow the vendor directory too, or index.html ends
# up pointing at files the binary does not carry.
inplace web/assets.go \
    -e 's|//go:embed index.html app.js style.css$|//go:embed index.html app.js style.css vendor|'

grep -q 'go:embed index.html app.js style.css vendor' web/assets.go || {
    echo "vendor-js: assets.go still does not embed web/vendor" >&2
    exit 1
}
echo "vendor-js: web/vendor/ in place and embedded"
