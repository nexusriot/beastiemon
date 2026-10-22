VERSION  ?= 0.2.0
PREFIX   ?= /usr/local
DESTDIR  ?=
GOOS     ?= freebsd
GOARCH   ?= amd64
# The ABI a package declares. pkg(8) refuses one built for another major
# release, so a host on FreeBSD 14 wants ABI=FreeBSD:14:amd64.
ABI      ?= FreeBSD:$(ABI_VERSION):amd64
# FreeBSD major release the packages target.
ABI_VERSION ?= 15
STAGE    := $(CURDIR)/.stage
PKGDIR   := $(CURDIR)/.pkg
DIST     := $(CURDIR)/dist

# Architectures `make dist` cuts packages for.
ARCHES   ?= amd64 arm64

# uPlot version pinned for reproducible builds.
UPLOT_VER ?= 1.6.27

E2E_IMAGE ?= beastiemon-e2e:$(VERSION)

# Shared by pkg-docker and dist-docker. --output writes the final stage into
# the working tree instead of building an image, so the artefacts arrive with
# the caller's ownership and nothing has to be cleaned up afterwards.
DOCKER_PKG = docker build -f scripts/Dockerfile --target artifact \
	--build-arg VERSION=$(VERSION) \
	--build-arg GOOS=$(GOOS) \
	--build-arg GOARCH=$(GOARCH) \
	--build-arg ABI=$(ABI) \
	--build-arg ABI_VERSION=$(ABI_VERSION) \
	--build-arg ARCHES="$(ARCHES)" \
	--output type=local,dest=$(CURDIR) .

# The scripts read these from the environment, so the Makefile and a direct
# `sh scripts/...` invocation agree on one set of defaults. GOOS/GOARCH are
# deliberately NOT exported: they name the *package* target, and exporting
# them would make `make test` build the test binaries for FreeBSD and then try
# to run them on the build host. The targets that cross-compile pass them in.
# ABI is not exported either: pkg(8) reads an ABI environment variable itself
# and warns about a missing OSVERSION when it finds one.
export VERSION PREFIX STAGE PKGDIR UPLOT_VER ARCHES

.PHONY: all build build-native dist vendor-js deps stage install pkg \
        pkg-docker dist-docker clean run lint fmt test e2e e2e-local

all: deps vendor-js build

deps:
	go mod download
	go mod tidy

# Download uPlot so the binary is self-contained (no CDN dependency).
vendor-js:
	sh scripts/vendor-js.sh

# Cross-compile for $(GOOS)/$(GOARCH) into dist/<goos>-<goarch>/.
build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) sh scripts/build.sh

# Compile for this machine (handy on a Linux dev box).
build-native:
	sh scripts/build.sh

# Every shipped architecture, packaged, with a SHA256 manifest.
dist: vendor-js
	GOOS=$(GOOS) ABI_VERSION=$(ABI_VERSION) sh scripts/release.sh

stage: build
	GOOS=$(GOOS) GOARCH=$(GOARCH) sh scripts/stage.sh

install: stage
	cp -R $(STAGE)/* $(DESTDIR)/

pkg: stage
	ABI=$(ABI) sh scripts/mkpkg.sh

# The same package, built in a container: the host needs Docker and nothing
# else — no Go, no FreeBSD. Output lands in .pkg/ and dist/ owned by whoever
# ran make, and no image or container is left behind.
pkg-docker:
	$(DOCKER_PKG) --build-arg MAKE_TARGET=pkg

# Every architecture in ARCHES, packaged in a container, with checksums.
dist-docker:
	$(DOCKER_PKG) --build-arg MAKE_TARGET=dist

clean:
	rm -rf $(STAGE) $(PKGDIR) $(DIST) beastied beastie

run: build-native
	$(DIST)/$$(go env GOHOSTOS)-$$(go env GOHOSTARCH)/beastied \
	    -config freebsd/beastiemon.conf

lint:
	go vet ./...
	GOOS=$(GOOS) go vet ./...
	go vet -tags e2e ./test/...

fmt:
	gofmt -w .

test:
	go test ./...

# End-to-end against a real daemon, inside a container: no Go, no FreeBSD and
# no free port needed on the host, and nothing it starts outlives the run.
e2e:
	docker build -f test/e2e/Dockerfile -t $(E2E_IMAGE) .
	docker run --rm $(E2E_IMAGE)

# The same suite against the host's Go, for iterating without a rebuild.
e2e-local:
	go test -tags e2e -count=1 -v ./test/e2e
