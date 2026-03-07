# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Install CA certificates in the builder so we can copy them to scratch.
RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o replic2 .

# Final stage — busybox:musl gives us a minimal shell (sh, ls, find, cat, …)
# while keeping the image tiny (~2 MB base vs scratch).
# CA certificates are copied from the builder for Kubernetes API server TLS.
FROM busybox:musl

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/replic2 /replic2

EXPOSE 8080

ENTRYPOINT ["/replic2"]
