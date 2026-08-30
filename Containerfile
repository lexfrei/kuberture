FROM golang:1.27@sha256:4013ae0f9e7994f8535c58c811f8f863fbed38b72e0d51e6592156f758d66146 AS builder
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

FROM gcr.io/distroless/static:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
LABEL org.opencontainers.image.source="https://github.com/lexfrei/kuberture"
LABEL org.opencontainers.image.description="Kubernetes EndpointSlice to DNS controller"
LABEL org.opencontainers.image.licenses="BSD-3-Clause"
LABEL org.opencontainers.image.title="kuberture"
COPY --from=builder /workspace/kuberture /kuberture
USER 65532:65532
ENTRYPOINT ["/kuberture"]
