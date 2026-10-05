FROM --platform=linux/amd64 golang:1.21-bookworm AS build

WORKDIR /src

COPY go.mod ./
COPY main.go ./

RUN set -xe; \
    CGO_ENABLED=1 \
    go build \
      -buildmode=pie \
      -ldflags "-linkmode external -extldflags '-static-pie'" \
      -tags netgo \
      -o /server \
      . \
    ;

FROM scratch

COPY --from=build /server /server
COPY --from=build /lib/x86_64-linux-gnu/libc.so.6 /lib/x86_64-linux-gnu/
COPY --from=build /lib64/ld-linux-x86-64.so.2 /lib64/
