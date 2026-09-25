# Every target runs inside pinned container images; nothing is installed on
# the host. The Go module and build caches live outside the repository.

VERSION        := 0.1.0
GO_IMAGE       := golang:1.26.8-bookworm@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81
LINT_IMAGE     := golangci/golangci-lint:v2.13.2@sha256:ba07dffad130794ae79ebaa0056809d18c0168f3f846480ffd3eb6c04578b83d
GITLEAKS_IMAGE := zricethezav/gitleaks:v8.30.1@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f

CACHE_DIR ?= $(HOME)/.cache/cpa-groq-build
DIST      := dist/linux-amd64
ARTIFACT  := $(DIST)/cpa-groq-v$(VERSION).so

DOCKER_RUN = docker run --rm --network=bridge \
	--user $$(id -u):$$(id -g) \
	-e HOME=/tmp -e GOFLAGS=-mod=readonly -e GOTOOLCHAIN=local -e CGO_ENABLED=1 \
	-e GOCACHE=/cache/go-build -e GOMODCACHE=/cache/gomod -e GOLANGCI_LINT_CACHE=/cache/golangci \
	-v $(CURDIR):/src -v $(CACHE_DIR):/cache -w /src

.PHONY: all build test vet lint check gitleaks clean checksum

all: check build

$(CACHE_DIR):
	mkdir -p $(CACHE_DIR)

# Reproducible c-shared build: trimmed paths, no build id, VCS stamping on.
build: | $(CACHE_DIR)
	@grep -q 'pluginVersion = "$(VERSION)"' models.go || { echo "models.go pluginVersion != $(VERSION)"; exit 1; }
	mkdir -p $(DIST)
	$(DOCKER_RUN) $(GO_IMAGE) go build -buildmode=c-shared -trimpath -buildvcs=true \
		-ldflags='-s -w -buildid=' -o $(ARTIFACT) .
	rm -f $(DIST)/cpa-groq-v$(VERSION).h
	$(MAKE) --no-print-directory checksum

checksum:
	cd $(DIST) && sha256sum cpa-groq-v$(VERSION).so | tee cpa-groq-v$(VERSION).so.sha256

test: | $(CACHE_DIR)
	$(DOCKER_RUN) $(GO_IMAGE) go test -race -count=1 ./...
	$(DOCKER_RUN) $(GO_IMAGE) go test -race -count=1 -tags cabitest ./...

vet: | $(CACHE_DIR)
	$(DOCKER_RUN) $(GO_IMAGE) go vet ./...
	$(DOCKER_RUN) $(GO_IMAGE) go vet -tags cabitest ./...

lint: | $(CACHE_DIR)
	$(DOCKER_RUN) $(LINT_IMAGE) golangci-lint run ./...
	$(DOCKER_RUN) $(LINT_IMAGE) golangci-lint run --build-tags cabitest ./...

check: vet lint test

# Scans the working tree and the full git history.
gitleaks:
	docker run --rm --network=none -v $(CURDIR):/repo:ro $(GITLEAKS_IMAGE) git --no-banner --redact -v /repo
	docker run --rm --network=none -v $(CURDIR):/repo:ro $(GITLEAKS_IMAGE) dir --no-banner --redact -v /repo

clean:
	rm -rf dist
