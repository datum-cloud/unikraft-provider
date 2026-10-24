# syntax=docker/dockerfile:1
# The OCI adapter: runs on every Unikraft node, pulls plain OCI images
# through the node's containerd, flattens them into the runtime's EROFS image
# format with mkfs.erofs and serves them to the runtime's image agent over
# http+oci. See internal/imageconvert.
#
# Build (from the repo root, which holds go.mod, cmd/ and internal/):
#   docker build -f build/ukp-oci-adapter/Dockerfile -t ukp-oci-adapter:TAG .
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod/ \
    go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN --mount=type=cache,target=/go/pkg/mod/ \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -o ukp-oci-adapter ./cmd/ukp-oci-adapter

FROM alpine:3.23

# Every node must build the same bytes for the same image, so mkfs.erofs is
# pinned. A new version can change converted images: bump imageconvert.Format
# with it.
ARG EROFS_UTILS_VERSION=1.8.10-r1
RUN apk add --no-cache erofs-utils="${EROFS_UTILS_VERSION}"
COPY --from=builder /workspace/ukp-oci-adapter /usr/local/bin/ukp-oci-adapter
ENTRYPOINT ["/usr/local/bin/ukp-oci-adapter"]
