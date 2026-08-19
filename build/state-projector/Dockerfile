# syntax=docker/dockerfile:1
# The per-second billing state-projector: a small node agent that turns the
# runtime's vm.state_change lifecycle stream into project-attributed usage
# windows for the edge Vector agent to meter. Builds the static sidecar binary
# from this repo and runs it as root — like the other ukp-runtime node
# containers, it writes to root-owned hostPaths (/var/run/ukp).
#
# Build:
#   docker build -t ukp-state-projector:TAG build/state-projector/
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
    go build -o state-projector ./cmd/state-projector

# distroless/static, run as root: like the other ukp-runtime node containers,
# the projector must write the root-owned ukp-run hostPath (/var/run/ukp) where
# the usage stream lives and read the root-owned ukp-data hostPath.
FROM gcr.io/distroless/static
WORKDIR /
COPY --from=builder /workspace/state-projector .
USER 0:0
ENTRYPOINT ["/state-projector"]
