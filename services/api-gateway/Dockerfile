# syntax=docker/dockerfile:1.7
# api-gateway image. Build context: repository root (go.work). Multi-stage, static binary, distroless non-root.
# Base images are pinned by digest (tag kept for readability); maintainers re-resolve digests when updating.
FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOWORK=off
COPY gen/go.mod gen/go.sum ./gen/
COPY pkg/go.mod pkg/go.sum ./pkg/
COPY services/api-gateway/go.mod services/api-gateway/go.sum ./services/api-gateway/
WORKDIR /src/services/api-gateway
RUN --mount=type=cache,target=/go/pkg/mod go mod download
WORKDIR /src
COPY gen ./gen
COPY pkg ./pkg
COPY services/api-gateway ./services/api-gateway
WORKDIR /src/services/api-gateway
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w" -o /out/api-gateway ./cmd/api-gateway

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/api-gateway /api-gateway
ARG REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/zodixdev/infrapilot" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.title="infrapilot-api-gateway" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 65532:65532
EXPOSE 8080 8081
ENTRYPOINT ["/api-gateway"]
