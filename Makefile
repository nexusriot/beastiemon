VERSION  ?= 0.1.0
PREFIX   ?= /usr/local
DESTDIR  ?=
GOOS     ?= freebsd
GOARCH   ?= amd64
STAGE    := $(CURDIR)/.stage
PKGDIR   := $(CURDIR)/.pkg

LDFLAGS  := -s -w -X main.version=$(VERSION)
GOFLAGS  := GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0

# uPlot version pinned for reproducible builds.
UPLOT_VER := 1.6.27
UPLOT_URL := https://unpkg.com/uplot@$(UPLOT_VER)/dist

.PHONY: all build vendor-js deps stage install pkg clean

all: deps vendor-js build

deps:
	go mod download
	go mod tidy

# Download uPlot so the binary is self-contained (no CDN dependency).
vendor-js:
	@mkdir -p web/vendor
	@echo "Fetching uPlot $(UPLOT_VER)…"
	fetch -q -o web/vendor/uplot.iife.min.js \
	    $(UPLOT_URL)/uPlot.iife.min.js 2>/dev/null || \
	curl -fsSL -o web/vendor/uplot.iife.min.js \
	    $(UPLOT_URL)/uPlot.iife.min.js
	fetch -q -o web/vendor/uplot.min.css \
	    $(UPLOT_URL)/uPlot.min.css 2>/dev/null || \
	curl -fsSL -o web/vendor/uplot.min.css \
	    $(UPLOT_URL)/uPlot.min.css
	# Rewrite CDN refs in index.html to local vendor copies.
	sed -i '' \
	    -e 's|https://unpkg.com/uplot@[^/]*/dist/uPlot.iife.min.js|vendor/uplot.iife.min.js|g' \
	    -e 's|https://unpkg.com/uplot@[^/]*/dist/uPlot.min.css|vendor/uplot.min.css|g' \
	    web/index.html
	# Ensure assets.go embeds the vendor directory too.
	sed -i '' 's|//go:embed index.html app.js style.css|//go:embed index.html app.js style.css vendor|' \
	    web/assets.go

build:
	$(GOFLAGS) go build -trimpath -ldflags '$(LDFLAGS)' \
	    -o beastied ./cmd/beastied
	$(GOFLAGS) go build -trimpath -ldflags '$(LDFLAGS)' \
	    -o beastie  ./cmd/beastie

build-native:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' \
	    -o beastied ./cmd/beastied
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' \
	    -o beastie  ./cmd/beastie

stage: build
	@rm -rf $(STAGE)
	install -d $(STAGE)$(PREFIX)/bin
	install -d $(STAGE)$(PREFIX)/etc/rc.d
	install -d $(STAGE)$(PREFIX)/etc
	install -m 0755 beastied $(STAGE)$(PREFIX)/bin/beastied
	install -m 0755 beastie  $(STAGE)$(PREFIX)/bin/beastie
	sed 's|%%PREFIX%%|$(PREFIX)|g' freebsd/beastied.in \
	    > $(STAGE)$(PREFIX)/etc/rc.d/beastied
	chmod 0755 $(STAGE)$(PREFIX)/etc/rc.d/beastied
	install -m 0644 freebsd/beastiemon.conf \
	    $(STAGE)$(PREFIX)/etc/beastiemon.conf.sample

install: stage
	cp -R $(STAGE)/* $(DESTDIR)/

pkg: stage
	@mkdir -p $(PKGDIR)
	# Write manifest with version substituted.
	sed 's/%%VERSION%%/$(VERSION)/' freebsd/+MANIFEST \
	    > $(STAGE)/+MANIFEST
	cp freebsd/pkg-descr $(STAGE)/+DESC
	pkg create \
	    --format txz \
	    --manifest $(STAGE)/+MANIFEST \
	    --root-dir $(STAGE) \
	    --out-dir $(PKGDIR)
	@echo ""
	@echo "Package: $(PKGDIR)/beastiemon-$(VERSION).pkg"
	@echo "Install: pkg install $(PKGDIR)/beastiemon-$(VERSION).pkg"

clean:
	rm -rf $(STAGE) $(PKGDIR) beastied beastie

run: build-native
	./beastied -config freebsd/beastiemon.conf

lint:
	go vet ./...

fmt:
	gofmt -w .

test:
	go test ./...
