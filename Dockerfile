FROM --platform=$BUILDPLATFORM golang:1.27.3 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY main.go main.go
COPY controllers/ controllers/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o probemgr main.go

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /app/probemgr .
USER 65532:65532

ENTRYPOINT ["/probemgr"]
