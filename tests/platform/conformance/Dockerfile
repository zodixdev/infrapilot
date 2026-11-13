# syntax=docker/dockerfile:1.7
# Build context: repository root (go.work). Multi-stage, static binary, distroless non-root.
# Base images are pinned by digest (tag kept for readability); maintainers re-resolve digests when updating.
FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.work go.work.sum ./
COPY gen/go.mod gen/go.sum ./gen/
COPY pkg/go.mod pkg/go.sum ./pkg/
COPY tests/go.mod tests/go.sum ./tests/
COPY tools/go.mod tools/go.sum ./tools/
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY gen ./gen
COPY pkg ./pkg
COPY tests ./tests
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/conformance ./tests/platform/conformance

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/conformance /conformance
USER 65532:65532
EXPOSE 8080 8081 9090
ENTRYPOINT ["/conformance"]
