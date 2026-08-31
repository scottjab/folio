# Everything here assumes `nix develop`, which pins Go 1.27 and Node 24.
# Nothing depends on what happens to be on your PATH.

.PHONY: all build embed test race lint fmt web check clean dev doctor nix-hashes

all: web build

## build: compile the binary with the frontend embedded
# Output goes under bin/ rather than the repo root. A binary at the root is
# named after the app, so it has to be listed in .gitignore by name, and a
# rename then silently un-ignores the old one. Ignoring a directory does not
# have that failure mode.
build: embed
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/folio ./cmd/folio

## embed: copy the bundle into the package go:embed reads
# Copies into dist/ rather than replacing it, so the tracked .gitkeep survives.
embed: web
	cp -r web/dist/. internal/web/dist/

## web: bundle the browser app
web:
	cd web && npm ci --no-audit --no-fund && npm run build

## test: run the Go and frontend test suites
test:
	go test ./...
	cd web && npm test

## race: the Go suite under the race detector
race:
	go test -race ./...

## lint: vet, staticcheck, and the frontend typecheck
lint:
	go vet ./...
	staticcheck ./... || true
	cd web && npm run check

## fmt: format Go sources
fmt:
	gofmt -w $$(git ls-files '*.go')

## check: what CI runs
check: lint test race

## dev: run locally with a fixed identity, no tailnet needed
dev: embed
	go run ./cmd/folio dev -log-level debug

## doctor: report on a state directory
doctor:
	go run ./cmd/folio doctor -state ./dev-state

## nix-hashes: refresh the two hashes the flake pins
# Any change to package-lock.json or go.mod invalidates one of these, and the
# failure message from nix is not obvious the first time you see it.
nix-hashes:
	@echo "npmDepsHash:"
	@nix shell nixpkgs#prefetch-npm-deps -c prefetch-npm-deps web/package-lock.json
	@echo
	@echo "vendorHash: build and read the 'got:' line from the mismatch"
	@echo "  sed -i 's|vendorHash = \".*\"|vendorHash = \"$(LIB_FAKE)\"|' flake.nix && nix build .#"

LIB_FAKE = sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=

clean:
	rm -rf bin result dev-state web/dist web/node_modules
	find internal/web/dist -mindepth 1 ! -name .gitkeep -delete
