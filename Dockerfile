# syntax=docker/dockerfile:1

# -- Stage 1: Builder
FROM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS builder
RUN apk add --no-cache libcap
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Release version reported at startup and by -version; image.yml passes it for main builds
ARG VERSION=dev
# Build static binary, ensuring CGO is disabled
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-w -s -X github.com/hononeko/thread-udp-proxy/pkg/version.Version=${VERSION}" -o /out/thread-udp-proxy ./cmd/proxy
# The raw packet socket needs CAP_NET_RAW. As a file capability it lets the nonroot user open it, as long as the
# container keeps NET_RAW in its capability set (Docker and containerd do by default; add it back after drop: ALL).
RUN setcap cap_net_raw=+ep /out/thread-udp-proxy

# -- Stage 2: Final Image (minimal static distroless)
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
WORKDIR /

# Copy compiled binary (with its file capability) from builder
COPY --from=builder /out/thread-udp-proxy /app/thread-udp-proxy

# Adhere to rootless mandates. Numeric (distroless "nonroot") so Kubernetes can verify runAsNonRoot.
USER 65532:65532

EXPOSE 9090

ENTRYPOINT ["/app/thread-udp-proxy"]
