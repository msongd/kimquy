# Makefile for Kim Quy (macOS 14+, Apple Silicon). Needs GNU make 3.81+
# (the one in Xcode's command line tools), Go and Xcode.
#
#   make            list the targets
#   make build      dev binaries in bin/ plus semd-embed
#   make test       Go tests (root module and ui/) with the race detector
#   make app        signed "packaging/out/Kim Quy.app" (no dmg)
#   make dmg        the app plus packaging/out/KimQuy-$(VERSION).dmg
#
# build.sh reads these (empty = its default):
#   VERSION=0.1.0  SIGN_ID="Developer ID Application: … (TEAMID)"
#   NOTARY_PROFILE=<keychain profile>  OUT=packaging/out
#   BUNDLE_MODELS=0 (slim app, the model downloads at first launch) | 1 (bundle MODELS)
#   MODELS=models/release

SHELL := /bin/bash
.DEFAULT_GOAL := help

BIN := bin
EMBEDDER := embedder
PRODUCTS := $(EMBEDDER)/.build-xcode/Build/Products/Release
MIN_MACOS := 14.0

export VERSION SIGN_ID NOTARY_PROFILE BUNDLE_MODELS MODELS OUT

# Same toolchain settings as packaging/build.sh.
# The -mmacosx-version-min flags matter: Go's build cache ignores
# MACOSX_DEPLOYMENT_TARGET, so without them it reuses cgo objects built for
# the host's macOS.
GOENV := GOOS=darwin GOARCH=arm64 MACOSX_DEPLOYMENT_TARGET=$(MIN_MACOS) \
	CGO_CFLAGS="-O2 -g -mmacosx-version-min=$(MIN_MACOS)" CGO_LDFLAGS="-mmacosx-version-min=$(MIN_MACOS)"

.PHONY: help check build go embed icon test vet app dmg clean distclean

help:
	@echo "targets:"
	@echo "  build      bin/semd, bin/semctl, bin/KimQuy and semd-embed (Swift)"
	@echo "  go         only the Go binaries"
	@echo "  embed      only semd-embed (xcodebuild, into $(PRODUCTS))"
	@echo "  icon       bin/AppIcon.icns and bin/AppIcon.png, to look at (app makes its own)"
	@echo "  test       go vet and go test -race in both modules"
	@echo "  vet        go vet in both modules"
	@echo "  app        packaging/out/Kim Quy.app, signed, no dmg; slim (it downloads the"
	@echo "             model at first launch) unless BUNDLE_MODELS=1"
	@echo "  dmg        app and dmg (notarized when NOTARY_PROFILE is set)"
	@echo "  clean      remove bin/ and packaging/out/"
	@echo "  distclean  clean, and the Swift build products too"
	@echo "variables: VERSION SIGN_ID NOTARY_PROFILE BUNDLE_MODELS MODELS OUT (see packaging/build.sh)"

# Fail early with a clear message instead of halfway through a build.
check:
	@[[ $$(uname -s) == Darwin && $$(uname -m) == arm64 ]] || { echo "make: needs macOS on Apple Silicon" >&2; exit 1; }
	@for t in go xcodebuild; do command -v $$t >/dev/null || { echo "make: $$t not found" >&2; exit 1; }; done

build: go embed

go: check
	mkdir -p $(BIN)
	$(GOENV) CGO_ENABLED=1 go build -o $(BIN)/semd ./cmd/semd
	$(GOENV) CGO_ENABLED=1 go build -o $(BIN)/semctl ./cmd/semctl
	cd ui && $(GOENV) CGO_ENABLED=0 go build -o ../$(BIN)/KimQuy .
	@echo "dev semd: $(BIN)/semd -sidecar $(PRODUCTS)/semd-embed -model models/release -state <dir>"

embed: check
	cd $(EMBEDDER) && xcodebuild -quiet -scheme semd-embed -configuration Release \
		-destination 'platform=macOS,arch=arm64' -derivedDataPath .build-xcode \
		CLANG_ENABLE_CODE_COVERAGE=NO CLANG_COVERAGE_MAPPING=NO SWIFT_ENABLE_CODE_COVERAGE=NO build
	@[[ -x $(PRODUCTS)/semd-embed ]] || { echo "make: semd-embed was not built" >&2; exit 1; }

icon: check
	rm -rf $(BIN)/AppIcon.iconset
	go run ./packaging/icon -out $(BIN)/AppIcon.iconset
	iconutil -c icns -o $(BIN)/AppIcon.icns $(BIN)/AppIcon.iconset
	cp $(BIN)/AppIcon.iconset/icon_512x512@2x.png $(BIN)/AppIcon.png

vet: check
	go vet ./...
	cd ui && go vet ./...

test: vet
	go test -race -count=1 ./...
	cd ui && go test -race -count=1 ./...

app: check
	NO_DMG=1 packaging/build.sh

dmg: check
	NO_DMG=0 packaging/build.sh

clean:
	rm -rf $(BIN) packaging/out

distclean: clean
	rm -rf $(EMBEDDER)/.build-xcode $(EMBEDDER)/.build
