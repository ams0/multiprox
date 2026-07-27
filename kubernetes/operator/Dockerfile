# ── Build stage ───────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS builder

WORKDIR /workspace

# Cache dependency downloads before copying source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# TARGETARCH/TARGETOS are supplied by BuildKit from the --platform of the
# target image. Do NOT hardcode amd64 here: the operator has to match the
# architecture of the cluster nodes it runs on, and arm64 clusters (Apple
# silicon, Graviton, kind on an M-series Mac) are common.
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev

# Statically linked binary for the distroless runtime image.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o manager \
    .

# ── Runtime stage ─────────────────────────────────────────────────────────────
# distroless/static has no shell — just the binary and CA certs.
# Use distroless/base if you need CGO or dynamic libraries.
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /workspace/manager .

# Run as non-root user (UID 65532 = nonroot in distroless).
USER 65532:65532

ENTRYPOINT ["/manager"]
