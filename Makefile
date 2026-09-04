VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/ewhauser/cursor-controller
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet lint image helm-template clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/cursor-controller ./cmd/cursor-controller

test:
	go test -race ./...

vet:
	go vet ./...

lint: vet
	test -z "$$(gofmt -l .)"

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

helm-template:
	helm template cursor-controller deploy/helm/cursor-controller \
	  --set auth.existingSecret=cursor-api-key \
	  --set worker.image.repository=example.local/cursor-worker \
	  --set worker.image.tag=test

clean:
	rm -rf bin

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
