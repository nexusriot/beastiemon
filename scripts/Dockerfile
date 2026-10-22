# Cut the FreeBSD package without a toolchain on the host: `make pkg-docker`.
#
# The package is built the same way `make pkg` builds it, by the same scripts
# — the container only supplies Go. Since no Linux image carries pkg(8),
# scripts/mkpkg.sh takes its portable path and writes the package directly;
# that is also what a cross-architecture build does on a FreeBSD host, so this
# is the same artefact, not a lesser one.
#
# The last stage holds nothing but the build's output, which is how
# `--output type=local` can drop it straight into the working tree with the
# invoking user's ownership — no bind mount, no root-owned files, no container
# left behind.
FROM golang:1.23-bookworm AS build

# Mirrors of the Makefile's knobs, passed through by the pkg-docker target.
ARG VERSION=0.2.0
ARG GOOS=freebsd
ARG GOARCH=amd64
ARG ABI=FreeBSD:15:amd64
ARG ABI_VERSION=15
ARG ARCHES="amd64 arm64"
# `pkg` for one architecture, `dist` for every one in ARCHES.
ARG MAKE_TARGET=pkg

WORKDIR /src

# Dependencies first, so editing the source does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Going through make rather than calling the scripts directly keeps this on
# exactly the path a host build takes, including anything added to it later.
RUN make "$MAKE_TARGET" \
        VERSION="$VERSION" \
        GOOS="$GOOS" \
        GOARCH="$GOARCH" \
        ABI="$ABI" \
        ABI_VERSION="$ABI_VERSION" \
        ARCHES="$ARCHES"

# Only the build's output travels back, laid out as a host build leaves it.
FROM scratch AS artifact
COPY --from=build /src/.pkg/ /.pkg/
COPY --from=build /src/dist/ /dist/
