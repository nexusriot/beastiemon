#!/bin/sh
#
# Run the end-to-end suite, FreeBSD half included, on a FreeBSD host over ssh.
#
#   E2E_HOST=root@freebsd-box scripts/e2e-remote.sh
#   E2E_HOST=root@freebsd-box E2E_RUN=FreeBSD scripts/e2e-remote.sh   # -run filter
#
# Copies the working tree (uncommitted changes included) into a fresh temp
# directory on the host, runs `go test -tags e2e` there as the ssh user, and
# removes the directory afterwards whatever the result. The host needs Go and
# key-based ssh; the FreeBSD tests also need that user to be root, and skip
# the parts whose tooling is missing (no zfs.ko, no cc, no VT-x for vmm).
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

: "${E2E_HOST:?set E2E_HOST=user@host}"
RUN=${E2E_RUN:-}

# The tarball is ssh's stdin and the remote script is its command: tar reads
# stdin to EOF, so nothing the tests start afterwards can hold the session
# open. The script is one line under sh -c because ssh hands it to the login
# shell, which for root may be csh — and csh allows no newlines inside quotes.
case $RUN in *"'"*|*'"'*) echo "e2e-remote: E2E_RUN must not contain quotes" >&2; exit 2 ;; esac
remote="set -eu; RUN=\"$RUN\"; dir=\$(mktemp -d /tmp/beastiemon-e2e-src.XXXXXX)"
remote="$remote; trap 'rm -rf \"\$dir\"' EXIT; tar -xzf - -C \"\$dir\"; cd \"\$dir\""
remote="$remote; uname -sr; go version"
remote="$remote; go test -tags e2e -count=1 -v \${RUN:+-run \"\$RUN\"} ./test/e2e"

# -T: no tty, so the remote side exits when the suite does.
tar --exclude=.git --exclude=dist --exclude=.stage -czf - . |
    ssh -T -o BatchMode=yes "$E2E_HOST" "sh -c '$(printf %s "$remote" | sed "s/'/'\\\\''/g")'"
