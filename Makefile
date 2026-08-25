VERSION ?= $(shell cat VERSION)
LDFLAGS = -X github.com/stronautt/orthogonals/internal/cli.Version=$(VERSION)

.PHONY: build test goldens lint test-integration test-vm test-vfio test-desk coverage coverage-pct rpm srpm srpm-lg lg-bump lg-checkout lg-lock

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

# `make lg-bump LG=B8` writes the version lockfile, moves the submodule, and
# regenerates the host-SHA lockfile. Omit LG= to re-sync the current version.
lg-bump:
	@if [ -n "$(LG)" ]; then printf '%s\n' "$(LG)" > internal/artifacts/looking-glass.version; fi
	$(MAKE) lg-checkout
	$(MAKE) lg-lock
	@echo "Looking Glass at $$(cat internal/artifacts/looking-glass.version) — review & commit"

lg-checkout:
	git -C $(LG_SRC) fetch --tags --quiet
	git -C $(LG_SRC) checkout --quiet $(LG_VERSION)

# curl -f aborts on failure, so a bad download never writes a bad pin.
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
	# test/desk is behind the desk build tag, so a plain `go vet ./...` never compiles it.
	go vet -tags desk ./test/desk
	go test ./...

# Only testsupport's importers accept -update, so `go test ./... -update` fails
# on every package that does not render an artifact. This list is that set.
goldens:
	go test ./internal/cli ./internal/domain ./internal/hostcfg ./internal/media -update

lint:
	golangci-lint run

# Outside the repo: tmt refuses a run workdir inside its own fmf root.
TMT_RUN = /var/tmp/orthogonals-tmt
test-integration:
	@command -v tmt >/dev/null || { echo "tmt required: dnf install tmt tmt+provision-container" >&2; exit 1; }
	rm -rf $(TMT_RUN)-container
	tmt run --all --id $(TMT_RUN)-container plan --name /plans/container

test-vm:
	@command -v tmt >/dev/null || { echo "tmt required: dnf install tmt tmt+provision-virtual" >&2; exit 1; }
	rm -rf $(TMT_RUN)-vm
	tmt run --all --id $(TMT_RUN)-vm plan --name /plans/vm

# tmt cannot provision this guest (testcloud has no way to ask for an emulated
# IOMMU), so test/vfiohost boots it and tmt connects to the address.
VFIO_KEY = /var/tmp/orthogonals-vfio/id_ed25519
test-vfio:
	@command -v tmt >/dev/null || { echo "tmt required: dnf install tmt" >&2; exit 1; }
	rm -rf $(TMT_RUN)-vfio
	guest=$$(go run ./test/vfiohost up) && \
	  tmt run --all --id $(TMT_RUN)-vfio plan --name /plans/vfio \
	    provision --how connect --guest "$$guest" --user root --key $(VFIO_KEY); \
	  status=$$?; go run ./test/vfiohost down; exit $$status

# The only check that the hand-written hwtest fixtures still match real
# hardware — no CI runner has the target GPU topology.
test-desk:
	@command -v tmt >/dev/null || { echo "tmt required: dnf install tmt" >&2; exit 1; }
	rm -rf $(TMT_RUN)-desk
	# --feeling-safe is tmt's guard on `provision: how: local`, which runs the
	# test on this machine rather than a throwaway guest; the plan only reads
	# (detect, preflight, status) and never applies.
	tmt --feeling-safe run --all --id $(TMT_RUN)-desk plan --name /plans/desk

# Merges the unit profile with the real binary's from any $(TMT_RUN)-* runs.
# A bare per-package -coverprofile undercounts, because it cannot see the
# *test helper packages being exercised from other packages' tests.
COVER = $(CURDIR)/dist/coverage
coverage:
	rm -rf $(COVER) && mkdir -p $(COVER)/unit $(COVER)/tier
	go test ./... -coverpkg=./internal/... -args -test.gocoverdir=$(COVER)/unit
	@find $(TMT_RUN)-container $(TMT_RUN)-vm $(TMT_RUN)-vfio -type d -name coverage 2>/dev/null \
		-exec cp -a '{}/.' $(COVER)/tier/ ';' || true
	go tool covdata textfmt -i=$(COVER)/unit -o=$(COVER)/unit.txt
	@if [ -n "$$(ls -A $(COVER)/tier 2>/dev/null)" ]; then \
		go tool covdata textfmt -i=$(COVER)/unit,$(COVER)/tier -o=$(COVER)/merged.txt; \
	else \
		echo "note: no tier coverage found — run make test-integration first"; \
		cp $(COVER)/unit.txt $(COVER)/merged.txt; \
	fi
	@# covdata merges two builds of the same package without complaint and keeps
	@# BOTH block layouts, so the stranger's blocks land in the denominator only
	@# and the figure drops several points with nothing said. Identical sources
	@# built by an identical toolchain give identical blocks, so a merge that
	@# adds an internal/ block proves one of the two drifted: the tier's only
	@# honest addition is main.go, which -coverpkg leaves out. Both shapes are
	@# silent otherwise — a stale $(TMT_RUN)-* from older sources (one moved
	@# comment is enough), or a tier binary built by another Go than this one.
	@u=$$(grep -c '/internal/' $(COVER)/unit.txt); \
	m=$$(grep -c '/internal/' $(COVER)/merged.txt); \
	if [ "$$m" -ne "$$u" ]; then \
		echo "error: tier coverage does not line up with this build:"; \
		echo "       merging it adds $$((m - u)) internal/ blocks to the unit profile's $$u."; \
		echo "       Either it ran on other sources — rm -rf $(TMT_RUN)-* and re-run the"; \
		echo "       tiers — or another Go built it: the tiers use Fedora's golang, so"; \
		echo "       whatever runs this target has to match it ($$(go env GOVERSION) here)."; \
		exit 1; \
	fi
	@printf 'total: (statements) %s%%\n' "$$($(MAKE) --no-print-directory coverage-pct)"

# The gated number. NOT `go tool cover -func`: it walks FuncDecls only, so
# package-level `var x = func(…)` seams — including both privilege-dropping
# exec paths — are absent from its total.
coverage-pct:
	@awk '!/^mode:/ { n = $$(NF-1); t += n; if ($$NF + 0 > 0) c += n } \
		END { if (t == 0) { print "0.0"; exit } printf "%.1f\n", 100 * c / t }' $(COVER)/merged.txt
