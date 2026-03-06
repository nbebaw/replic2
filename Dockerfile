# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Install CA certificates in the builder so we can copy them to scratch.
RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w" -o replic2 .

# Final stage — minimal image
# We use scratch (no OS) but must include:
#   - The static binary
#   - CA certificates (needed to verify the Kubernetes API server TLS cert)
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/replic2 /replic2

EXPOSE 8080

ENTRYPOINT ["/replic2"]
