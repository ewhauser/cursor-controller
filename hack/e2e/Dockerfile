# Test image with both fakes: fake-cursor-api and fake-worker. Runs as root so
# the fake worker can write to CSI hostpath volumes without fsGroup support.
# Build from the repository root: docker build -f hack/e2e/Dockerfile .
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/fake-cursor-api ./cmd/fake-cursor-api && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/fake-worker ./cmd/fake-worker

FROM gcr.io/distroless/static-debian12:latest
COPY --from=build /out/fake-cursor-api /fake-cursor-api
COPY --from=build /out/fake-worker /fake-worker
ENTRYPOINT ["/fake-worker"]
