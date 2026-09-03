# builder stage runs on host native platform (amd64), uses Go cross-compilation for target binary
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o omniproxy .

FROM alpine:3.21
RUN apk --no-cache add ca-certificates

WORKDIR /app
COPY --from=builder /app/omniproxy .
COPY --from=builder /app/web ./web
RUN mkdir -p /app/data && chown -R 65532:65532 /app

EXPOSE 8080
VOLUME /app/data

# Non-root. A bind-mounted ./data keeps its host ownership, so an existing
# deployment must either chown it to 65532 or override `user:` (see docker-compose.yml).
USER 65532:65532

CMD ["./omniproxy"]
