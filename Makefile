ENGINE ?= $(shell command -v podman || command -v docker)
VERSION ?= $(shell cat VERSION)
LDFLAGS = -X github.com/stronautt/orthogonals/internal/cli.Version=$(VERSION)

.PHONY: build test lint test-integration test-vm rpm srpm srpm-lg lg-bump lg-checkout lg-lock

# LG_VERSION is the single Looking Glass toggle (the looking-glass.version
# lockfile, embedded by artifacts.go); LG_RPMVER is its RPM form (0~ sorts the
# beta below any 1.0).
LG_VERSION := $(shell cat internal/artifacts/looking-glass.version)
LG_RPMVER := 0~$(LG_VERSION)
LG_SRC := packaging/third_party/LookingGlass

# Local convenience build: in-place, skips %prep, no tarball needed.
rpm:
	rpmbuild --build-in-place -bb packaging/orthogonals.spec \
		--define "pkgver $(VERSION)" \
		--define "lgver $(LG_RPMVER)" \
		--define "_rpmdir $(CURDIR)/dist"

# Source RPM for COPR: mock rebuilds it from the git-archive tarball.
# The version is baked into a generated spec — a runtime --define would
# not survive into the SRPM and the rebuild would fail. Go modules are
# vendored into the tarball only (never the repo): mock builds are
# network-free.
srpm:
	mkdir -p dist
	rm -rf dist/srpm-src
	mkdir -p dist/srpm-src
	git archive --prefix=orthogonals-$(VERSION)/ HEAD | tar -xf - -C dist/srpm-src
	cd dist/srpm-src/orthogonals-$(VERSION) && go mod vendor
	tar -czf dist/orthogonals-$(VERSION).tar.gz -C dist/srpm-src orthogonals-$(VERSION)
	rm -rf dist/srpm-src
	{ echo "%define pkgver $(VERSION)"; echo "%define lgver $(LG_RPMVER)"; \
		cat packaging/orthogonals.spec; } > dist/orthogonals.spec
	rpmbuild -bs dist/orthogonals.spec \
		--define "_sourcedir $(CURDIR)/dist" \
		--define "_srcrpmdir $(CURDIR)/dist"

# git archive does not recurse submodules, so tar the checked-out working tree;
# stamp VERSION because the tarball has no git state for LG to `git describe`.
srpm-lg:
	mkdir -p dist
	git submodule update --init --recursive $(LG_SRC)
	tag=$$(git -C $(LG_SRC) describe --tags --exact-match 2>/dev/null || true); \
		if [ -n "$$tag" ] && [ "$$tag" != "$(LG_VERSION)" ]; then \
			echo "$(LG_SRC) tag $$tag != $(LG_VERSION) (artifacts.go) — run 'make lg-checkout'" >&2; \
			exit 1; \
		fi
	rm -rf dist/lg-src
	mkdir -p dist/lg-src/looking-glass-client-$(LG_RPMVER)
	tar --exclude='.git' -C $(LG_SRC) -cf - . \
		| tar -xf - -C dist/lg-src/looking-glass-client-$(LG_RPMVER)
	echo $(LG_VERSION) > dist/lg-src/looking-glass-client-$(LG_RPMVER)/VERSION
	tar -czf dist/looking-glass-client-$(LG_RPMVER).tar.gz -C dist/lg-src looking-glass-client-$(LG_RPMVER)
	rm -rf dist/lg-src
	{ echo "%global lgver $(LG_RPMVER)"; cat packaging/looking-glass-client.spec; } \
		> dist/looking-glass-client.spec
	rpmbuild -bs dist/looking-glass-client.spec \
		--define "_sourcedir $(CURDIR)/dist" \
		--define "_srcrpmdir $(CURDIR)/dist"

# Bump the Looking Glass release: `make lg-bump LG=B8` writes the version
# lockfile, moves the submodule, and regenerates the host-SHA lockfile. Omit LG=
# to re-sync the current version. Review the diff, then commit.
lg-bump:
	@if [ -n "$(LG)" ]; then printf '%s\n' "$(LG)" > internal/artifacts/looking-glass.version; fi
	$(MAKE) lg-checkout
	$(MAKE) lg-lock
	@echo "Looking Glass at $$(cat internal/artifacts/looking-glass.version) — review & commit"

# Move the submodule to the pinned version.
lg-checkout:
	git -C $(LG_SRC) fetch --tags --quiet
	git -C $(LG_SRC) checkout --quiet $(LG_VERSION)

# Regenerate the host-zip SHA256 lockfile from the download for the pinned
# version. curl -f aborts on failure, so a bad download never writes a bad pin.
lg-lock:
	mkdir -p dist
	curl -fsSL -o dist/lg-host.zip "https://looking-glass.io/artifact/$(LG_VERSION)/host"
	sha256sum dist/lg-host.zip | cut -d' ' -f1 > internal/artifacts/looking-glass.sha256
	rm -f dist/lg-host.zip
	@echo "wrote looking-glass.sha256 for $(LG_VERSION)"

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o orthogonals .

test:
	go vet ./...
	go test ./...

lint:
	golangci-lint run

# Integration tier: full detect → preflight → apply --yes → undo cycle in a
# clean fedora:44 container against a synthetic --root tree + fake binaries.
test-integration:
	test -n "$(ENGINE)" || { echo "podman or docker required" >&2; exit 1; }
	CGO_ENABLED=0 GOOS=linux go build -o test/integration/orthogonals .
	rm -rf test/integration/fixture-root test/integration/fixture-root-laptop test/integration/fixture-root-laptop-amd
	go run ./test/fixture test/integration/fixture-root reference
	go run ./test/fixture test/integration/fixture-root-laptop laptop
	go run ./test/fixture test/integration/fixture-root-laptop-amd laptop-amd
	$(ENGINE) build -t orthogonals-integration -f test/integration/Containerfile test/integration
	$(ENGINE) run --rm orthogonals-integration

# System tier (local only, needs libvirt): real apply → reboot → assert →
# undo → assert pristine, on a throwaway Fedora Cloud VM.
test-vm:
	bash test/vm/run-test.sh
