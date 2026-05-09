FROM golang:1.26@sha256:257c1f60c465aa5d22b4d81f9ae73643a12f228a10165c658ec77bd6ff791f34 AS builder
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

FROM gcr.io/distroless/static:nonroot@sha256:e3f945647ffb95b5839c07038d64f9811adf17308b9121d8a2b87b6a22a80a39
LABEL org.opencontainers.image.source="https://github.com/lexfrei/kuberture"
LABEL org.opencontainers.image.description="Kubernetes EndpointSlice to DNS controller"
LABEL org.opencontainers.image.licenses="BSD-3-Clause"
LABEL org.opencontainers.image.title="kuberture"
COPY --from=builder /workspace/kuberture /kuberture
USER 65532:65532
ENTRYPOINT ["/kuberture"]
