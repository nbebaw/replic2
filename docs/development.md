---
layout: default
title: Development
nav_order: 5
---

# Development

## Prerequisites

- Go 1.24+
- Docker
- `kubectl` + a running cluster (kind recommended for local dev)
- `helm` 3.x

## Build

```bash
# Build binary
go build -o replic2 .

# Build optimised and stripped (matches the Dockerfile)
CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o replic2 .
```

## Run locally

replic2 falls back to `~/.kube/config` when running outside a cluster.

```bash
# Default port 8080
go run .

# Custom port
PORT=9090 go run .
```

S3 credentials must also be set as environment variables for the backup and restore controllers to function:

```bash
S3_ENDPOINT=http://localhost:9000 \
S3_BUCKET=replic2-backups \
S3_ACCESS_KEY_ID=replic2 \
S3_SECRET_ACCESS_KEY=replic2secret \
S3_USE_PATH_STYLE=true \
go run .
```

## Dependency management

```bash
go mod tidy
go mod download
```

## Testing

```bash
# Run all tests
go test ./...

# Verbose output
go test -v ./...

# Single test by name
go test -v -run TestFunctionName .

# Race detector
go test -race ./...

# Coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

Handler tests use `net/http/httptest`. Controller tests use `k8s.io/client-go/dynamic/fake`.

## Linting and formatting

```bash
# Format all code in place
gofmt -w .

# Check formatting without modifying
gofmt -l .

# Static analysis
go vet ./...
```

Always run `gofmt -w .` and `go vet ./...` before committing.

## CI/CD

GitHub Actions (`.github/workflows/build.yaml`) triggers on every push to `main`:

1. Builds the Docker image for `linux/amd64`.
2. Pushes to `ghcr.io/nbebaw/replic2` tagged `sha-<short-sha>` and `latest`.

CI does not run tests. Run them locally before pushing.

---

## Project structure

```
replic2/
├── main.go                                 entry point — wires clients, HTTP server, leader election, graceful shutdown
├── main_test.go                            HTTP handler integration tests
├── docs/                                   GitHub Pages documentation (Jekyll)
├── charts/replic2/                         Helm chart
│   ├── Chart.yaml
│   ├── values.yaml
│   └── templates/
│       ├── _helpers.tpl
│       ├── serviceaccount.yaml
│       ├── rbac.yaml
│       ├── secret-ghcr.yaml
│       ├── secret-s3.yaml
│       ├── deployment.yaml
│       ├── service.yaml
│       ├── ingress.yaml
│       ├── hpa.yaml
│       ├── poddisruptionbudget.yaml
│       └── crds/
│           ├── crd-backup.yaml
│           ├── crd-restore.yaml
│           └── crd-scheduledbackup.yaml
└── internal/
    ├── k8s/client.go                       Kubernetes client initialisation (in-cluster → kubeconfig fallback)
    ├── s3/client.go                        S3 client init from env vars
    ├── types/types.go                      CRD Go types (Backup, Restore, ScheduledBackup) + scheme registration
    ├── store/store.go                      S3 I/O helpers: PutObject, GetObject, ListKeys, DeletePrefix
    ├── leader/leader.go                    Lease-based leader election via client-go
    ├── server/
    │   ├── server.go                       HTTP router (gin) — route registration
    │   └── handler/
    │       ├── types.go                    Response structs (HelloResponse, HealthResponse, Response)
    │       ├── general.go                  Shared CR listing logic
    │       ├── backupApi.go                /backup handler
    │       ├── restoreApi.go               /restore handler
    │       ├── healthzApi.go               /healthz handler
    │       └── readyzApi.go                /readyz handler
    └── controller/
        ├── backup/
        │   ├── backup.go                   Poll loop, constants, exported wrappers
        │   ├── process.go                  process(), FindLatestCompletedBackup()
        │   ├── manifests.go                Resource type discovery, S3 PutObject for manifests
        │   ├── pvcdata.go                  Agent pod that tars PVC data and streams to S3
        │   └── status.go                   Status patching, TTL expiry, S3 DeletePrefix
        ├── restore/
        │   └── restore.go                  Manifest restore from S3; PVC data restore via agent pod
        └── scheduled/
            └── scheduled.go               ScheduledBackup controller — cron-based Backup CR creation
```

---

## Adding a new backed-up resource type

1. Add the `schema.GroupVersionResource` to the `resourceTypes` slice in `internal/controller/backup/manifests.go`.
2. Ensure the `ClusterRole` in `deploy/rbac.yaml` and `charts/replic2/templates/rbac.yaml` grants `get`/`list`/`create`/`patch`/`update` on that resource.
3. No changes are needed to the restore controller — it reads `resourceTypes` from the same slice.

## Adding a new HTTP endpoint

1. Define a response struct with JSON tags in `internal/server/handler/types.go`.
2. Create `internal/server/handler/<name>Api.go` with a `<Name>Handler(c *gin.Context, ...)` function.
3. If the handler lists CRs, define the GVR in the handler file and delegate to `General(c, clients, GVR)`.
4. Register the route in `server.NewRouter()` using a closure to inject dependencies.
5. Add tests in `main_test.go` using `net/http/httptest` and `newFakeClients()`.
