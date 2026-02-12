FROM golang:1.26@sha256:c83e68f3ebb6943a2904fa66348867d108119890a2c6a2e6f07b38d0eb6c25c5 AS builder
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
    -o kuberture ./cmd/kuberture

FROM gcr.io/distroless/static:nonroot@sha256:f9f84bd968430d7d35e8e6d55c40efb0b980829ec42920a49e60e65eac0d83fc
LABEL org.opencontainers.image.source="https://github.com/lexfrei/kuberture"
LABEL org.opencontainers.image.description="Kubernetes EndpointSlice to DNS controller"
LABEL org.opencontainers.image.licenses="BSD-3-Clause"
LABEL org.opencontainers.image.title="kuberture"
COPY --from=builder /workspace/kuberture /kuberture
USER 65532:65532
ENTRYPOINT ["/kuberture"]
