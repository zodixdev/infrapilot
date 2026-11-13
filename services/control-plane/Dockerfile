# syntax=docker/dockerfile:1.7
# control-plane image. Build context: repository root (go.work). Multi-stage, static binary, distroless non-root.
# Base images are pinned by digest (tag kept for readability); maintainers re-resolve digests when updating.
FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOWORK=off
COPY gen/go.mod gen/go.sum ./gen/
COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/control-plane/go.mod services/control-plane/go.sum ./services/control-plane/
WORKDIR /src/services/control-plane
RUN --mount=type=cache,target=/go/pkg/mod go mod download
WORKDIR /src
COPY gen ./gen
COPY pkg ./pkg
COPY services/control-plane ./services/control-plane
WORKDIR /src/services/control-plane
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/control-plane ./cmd/control-plane

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/control-plane /control-plane
ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/zodixdev/infrapilot" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.title="infrapilot-control-plane" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 65532:65532
EXPOSE 8081 9090
ENTRYPOINT ["/control-plane"]
