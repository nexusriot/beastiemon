# Hermetic end-to-end run: `make e2e`.
#
# The suite starts real beastied processes, binds ports, writes config files
# and SQLite databases, and sends signals. Doing that inside a container keeps
# it off the developer's machine and makes the result the same everywhere —
# the host needs Docker and nothing else, not even Go.
#
# The daemon's FreeBSD-only collectors (ZFS, jails, pf, temperatures) compile
# to stubs here, so what this proves is the portable surface: the HTTP API,
# streaming, auth, the store, the alert engine, the CLI, and config handling.
# The FreeBSD-specific half — rc.d, pkg(8), those collectors — is covered by
# the unit tests and by installing the package on a FreeBSD host.
FROM golang:1.23-bookworm

WORKDIR /src

# Dependencies first, so edits to the source do not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# The binaries under test, built the way a release is.
RUN CGO_ENABLED=0 go build -trimpath -o /out/beastied ./cmd/beastied \
 && CGO_ENABLED=0 go build -trimpath -o /out/beastie  ./cmd/beastie

# Compiling the suite here means `docker run` goes straight to testing.
RUN go test -c -tags e2e -o /out/e2e.test ./test/e2e

ENV BEASTIED_BIN=/out/beastied \
    BEASTIE_BIN=/out/beastie

# -test.v so a failure shows which case broke; -test.count=1 defeats caching.
ENTRYPOINT ["/out/e2e.test", "-test.v", "-test.count=1", "-test.timeout=15m"]
