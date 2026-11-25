FROM golang:1.27@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS builder
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

FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
LABEL org.opencontainers.image.source="https://github.com/lexfrei/kuberture"
LABEL org.opencontainers.image.description="Kubernetes EndpointSlice to DNS controller"
LABEL org.opencontainers.image.licenses="BSD-3-Clause"
LABEL org.opencontainers.image.title="kuberture"
COPY --from=builder /workspace/kuberture /kuberture
USER 65532:65532
ENTRYPOINT ["/kuberture"]
