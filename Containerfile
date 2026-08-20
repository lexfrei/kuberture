FROM golang:1.27@sha256:65b6f280bf050ec5af12716857e8ea8439d694dbba8f31ceeb7630670071f2bb AS builder
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

FROM gcr.io/distroless/static:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6
LABEL org.opencontainers.image.source="https://github.com/lexfrei/kuberture"
LABEL org.opencontainers.image.description="Kubernetes EndpointSlice to DNS controller"
LABEL org.opencontainers.image.licenses="BSD-3-Clause"
LABEL org.opencontainers.image.title="kuberture"
COPY --from=builder /workspace/kuberture /kuberture
USER 65532:65532
ENTRYPOINT ["/kuberture"]
