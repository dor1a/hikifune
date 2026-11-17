# syntax=docker/dockerfile:1
# Two targets from one build: "agent" and "server" (the default). Both binaries
# carry the same version, which the server checks against each agent.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X github.com/dor1a/hikifune/internal/buildinfo.Version=${VERSION}" \
      -o /out/ ./cmd/...

FROM scratch AS agent
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/hikifune-agent /hikifune-agent
USER 65532:65532
ENTRYPOINT ["/hikifune-agent"]

FROM scratch AS server
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/hikifune /hikifune
USER 65532:65532
ENV HIKIFUNE_LISTEN=:8869
EXPOSE 8869
HEALTHCHECK --interval=30s --timeout=5s CMD ["/hikifune", "healthcheck"]
ENTRYPOINT ["/hikifune"]
