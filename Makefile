BIN     := spillhistorie
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX  ?= /usr/local

.DEFAULT_GOAL := build
.PHONY: build install uninstall run clean release

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN) .

install: build
	install -Dm755 $(BIN) $(DESTDIR)$(PREFIX)/bin/$(BIN)

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BIN)

run: build
	./$(BIN)

clean:
	rm -f $(BIN)
	rm -rf dist/

release:
	@mkdir -p dist
	@for target in \
		linux/amd64 linux/arm64 \
		darwin/amd64 darwin/arm64 \
		windows/amd64; do \
		GOOS=$${target%/*} GOARCH=$${target#*/}; \
		EXT=""; [ "$$GOOS" = "windows" ] && EXT=".exe"; \
		OUT="dist/$(BIN)-$(VERSION)-$$GOOS-$$GOARCH$$EXT"; \
		echo "  building $$OUT"; \
		CGO_ENABLED=0 GOOS=$$GOOS GOARCH=$$GOARCH \
			go build -trimpath -ldflags="$(LDFLAGS)" -o "$$OUT" . || exit 1; \
	done
	cd dist && sha256sum * > checksums.txt
	@echo "done — dist/"
