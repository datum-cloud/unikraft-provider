# syntax=docker/dockerfile:1
# Build on the native host arch and cross-compile, so the Go toolchain never
# runs under emulation (the rootfs is built for linux/amd64 -> x86_64).
#
# Unikraft's elfloader only accepts a position-independent executable, and the
# FROM scratch rootfs ships no dynamic loader, so the binary must be a STATIC
# PIE. A pure-Go `-buildmode=pie` is a PIE but still records an INTERP header
# pointing at ld-linux, which is absent on scratch. Linking externally against
# the x86_64 cross toolchain with `-static-pie` produces a PIE with no
# interpreter and no shared-library dependencies.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc-x86-64-linux-gnu libc6-dev-amd64-cross \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=1 CC=x86_64-linux-gnu-gcc GOOS=linux GOARCH=amd64 \
    go build -buildmode=pie -trimpath \
    -ldflags='-s -w -linkmode external -extldflags "-static-pie"' -o /netdump .

FROM scratch
COPY --from=build /netdump /netdump
ENTRYPOINT ["/netdump"]
