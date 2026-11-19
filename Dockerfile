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
      -o /out/ ./cmd/... \
 && mkdir -p /out/data

FROM scratch AS agent
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/hikifune-agent /hikifune-agent
USER 65532:65532
ENV HIKIFUNE_AGENT_LISTEN=:8870
EXPOSE 8870
HEALTHCHECK --interval=30s --timeout=5s CMD ["/hikifune-agent", "healthcheck"]
ENTRYPOINT ["/hikifune-agent"]

FROM scratch AS server
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/hikifune /hikifune
# The state directory, owned by the server's user so a new volume is too.
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV HIKIFUNE_LISTEN=:8869 HIKIFUNE_DATA=/data
EXPOSE 8869
HEALTHCHECK --interval=30s --timeout=5s CMD ["/hikifune", "healthcheck"]
ENTRYPOINT ["/hikifune"]
