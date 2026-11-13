FROM --platform=$BUILDPLATFORM golang:1.27.3 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /app

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY main.go main.go
COPY controllers/ controllers/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
  go build -a -ldflags "-s -w -X main.version=${VERSION}" -o probe-controller main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /app/probe-controller .
USER 65532:65532

ENTRYPOINT ["/probe-controller"]
