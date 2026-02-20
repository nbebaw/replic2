# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o replic2 .

# Final stage — minimal image
FROM scratch

COPY --from=builder /app/replic2 /replic2

EXPOSE 8080

ENTRYPOINT ["/replic2"]
