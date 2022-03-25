FROM golang:alpine AS zetcd
RUN go install -trimpath -ldflags '-s -w -extldflags "-static"' github.com/etcd-io/zetcd/cmd/zkboom@latest github.com/etcd-io/zetcd/cmd/zkctl@latest
FROM quay.io/coreos/etcd:v3.6.7 AS etcd
FROM muicoder/burrow:latest AS cached
ARG TARGETARCH
COPY --from=etcd /usr/local/bin /usr/local/bin
COPY --from=zetcd /go/bin /usr/local/bin
COPY .git/$TARGETARCH/* /usr/local/bin/
# kcat on libcrypto1.1+libssl1.1
FROM alpine:3.23
RUN apk add --no-cache curl jq wget tzdata libcurl lz4-libs zstd-libs ca-certificates openssl
COPY --from=cached /usr/local/bin /usr/local/bin
