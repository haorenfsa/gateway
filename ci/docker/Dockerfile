FROM golang:1.22 as source1
# Create the data directory for eg
WORKDIR /go/src/github.com/zilliztech/envoy-gateway
COPY go.mod go.sum ./
COPY api  ./api
COPY internal  ./internal
COPY cmd  ./cmd
COPY proto  ./proto
RUN mkdir -p ./bin && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ./bin/envoy-gateway ./cmd/envoy-gateway/main.go

FROM busybox as source2
# Create the data directory for eg
RUN mkdir -p /var/lib/eg

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot@sha256:8dd8d3ca2cf283383304fd45a5c9c74d5f2cd9da8d3b077d720e264880077c65
ARG TARGETPLATFORM
COPY --from=source1 /go/src/github.com/zilliztech/envoy-gateway/bin/envoy-gateway /usr/local/bin/
COPY --from=source2 --chown=65532:65532 /var/lib /var/lib

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/envoy-gateway"]
