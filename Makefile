VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/ewhauser/cursor-controller
LDFLAGS := -s -w -X main.version=$(VERSION)
GO_PACKAGES := ./...
RACE ?= -race
GOLANGCI_LINT_VERSION ?= v2.11.3
GOLANGCI_LINT := GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: check build test vet lint lint-new image helm-lint helm-template local-e2e clean

check: build test lint helm-lint

build:
	go build $(GO_PACKAGES)
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/cursor-controller ./cmd/cursor-controller

test:
	go test $(RACE) $(GO_PACKAGES)

vet:
	go vet $(GO_PACKAGES)

lint:
	$(GOLANGCI_LINT) run ./...

lint-new:
	$(GOLANGCI_LINT) run --new-from-rev=HEAD ./...

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

helm-lint:
	helm lint deploy/helm/cursor-controller \
	  --set auth.existingSecret=k \
	  --set worker.image.repository=r \
	  --set worker.image.tag=t
	helm lint deploy/helm/cursor-controller \
	  --set auth.existingSecret=k \
	  --set worker.image.repository=r \
	  --set worker.image.tag=t \
	  --set persistence.enabled=true \
	  --set controller.warmIdle=1

helm-template:
	helm template cursor-controller deploy/helm/cursor-controller \
	  --set auth.existingSecret=cursor-api-key \
	  --set worker.image.repository=example.local/cursor-worker \
	  --set worker.image.tag=test

clean:
	rm -rf bin

local-e2e:
	./hack/local-e2e.sh

# --- local integration / e2e ------------------------------------------------
.PHONY: fake-api e2e-setup e2e e2e-teardown

# Run the fake Cursor API on :8081 for manual controller runs:
#   cursor-controller --api-url http://localhost:8081 --api-key dev ...
fake-api:
	go run ./cmd/fake-cursor-api --addr :8081 --api-key dev --heartbeat 5s

e2e-setup:
	./hack/e2e/setup.sh

e2e:
	go test -tags e2e ./test/e2e -v -count=1 -timeout 30m

e2e-teardown:
	./hack/e2e/teardown.sh
