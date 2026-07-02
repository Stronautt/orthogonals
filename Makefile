ENGINE ?= $(shell command -v podman || command -v docker)
VERSION ?= $(shell cat VERSION)
LDFLAGS = -X github.com/stronautt/orthogonals/internal/cli.Version=$(VERSION)

.PHONY: build test lint test-integration test-vm rpm srpm

# Local convenience build: in-place, skips %prep, no tarball needed.
rpm:
	rpmbuild --build-in-place -bb packaging/orthogonals.spec \
		--define "pkgver $(VERSION)" \
		--define "_rpmdir $(CURDIR)/dist"

# Source RPM for COPR: mock rebuilds it from the git-archive tarball.
# The version is baked into a generated spec — a runtime --define would
# not survive into the SRPM and the rebuild would fail.
srpm:
	mkdir -p dist
	git archive --format=tar.gz --prefix=orthogonals-$(VERSION)/ \
		-o dist/orthogonals-$(VERSION).tar.gz HEAD
	{ echo "%define pkgver $(VERSION)"; cat packaging/orthogonals.spec; } \
		> dist/orthogonals.spec
	rpmbuild -bs dist/orthogonals.spec \
		--define "_sourcedir $(CURDIR)/dist" \
		--define "_srcrpmdir $(CURDIR)/dist"

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
	rm -rf test/integration/fixture-root
	go run ./test/fixture test/integration/fixture-root
	$(ENGINE) build -t orthogonals-integration -f test/integration/Containerfile test/integration
	$(ENGINE) run --rm orthogonals-integration

# System tier (local only, needs libvirt): real apply → reboot → assert →
# undo → assert pristine, on a throwaway Fedora Cloud VM.
test-vm:
	bash test/vm/run-test.sh
