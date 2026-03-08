# replic2

A Kubernetes operator and HTTP server that provides namespace-scoped backup and restore via Custom Resources.

## Features

- **Backup controller** — watches `Backup` CRs; serialises namespace resources to YAML files on a PVC; optionally copies raw PVC data via a temporary agent pod; supports automatic full/incremental backup selection.
- **Restore controller** — watches `Restore` CRs; re-applies YAML files from the PVC into the cluster using server-side apply.
- **ScheduledBackup controller** — cron-based automatic backup creation.
- **Leader election** — Lease-based; only the elected pod runs controllers. Standby pods still serve HTTP.
- **HTTP API** — exposes metadata, health probes, and CR listings.

---

## HTTP Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/` | Application metadata (version, hostname, namespace, timestamp) |
| `GET` | `/healthz` | Liveness probe |
| `GET` | `/readyz` | Readiness probe |
| `GET` | `/backup` | List all `Backup` CRs (name, phase, completedAt) |
| `GET` | `/restore` | List all `Restore` CRs (name, phase, completedAt) |

---

## Custom Resources

### Backup a namespace

```yaml
# Minimal — manifests only, type auto-selected (Full on first run, Incremental after)
apiVersion: replic2.io/v1alpha1
kind: Backup
metadata:
  name: my-app-backup-01
spec:
  namespace: my-app
```

```yaml
# Full backup including raw PVC data, with a 7-day TTL
apiVersion: replic2.io/v1alpha1
kind: Backup
metadata:
  name: my-app-backup-full
spec:
  namespace: my-app
  type: Full           # "Full" | "Incremental" — omit to auto-select
  includePVCData: true # also back up raw files from every bound PVC
  ttl: 168h            # auto-delete after 7 days
```

```yaml
# Incremental backup — manifests + only PVC files changed since the last backup
apiVersion: replic2.io/v1alpha1
kind: Backup
metadata:
  name: my-app-backup-inc-01
spec:
  namespace: my-app
  type: Incremental
  includePVCData: true
```

```bash
kubectl apply -f backup.yaml
kubectl get backups
kubectl describe backup my-app-backup-01
```

#### Backup spec fields

| Field | Type | Default | Description |
|---|---|---|---|
| `namespace` | string | required | Namespace to back up |
| `type` | string | auto | `Full` or `Incremental`. Auto-selects Full on first run, Incremental after |
| `includePVCData` | bool | `false` | Also copy raw data from every bound PVC in the namespace |
| `ttl` | string | — | Go duration (e.g. `24h`). CR and PVC data are deleted after `completedAt + ttl` |

#### Backup status fields written by the controller

| Field | Description |
|---|---|
| `phase` | `Pending` → `InProgress` → `Completed` \| `Failed` |
| `backupType` | `Full` or `Incremental` — what was actually performed |
| `basedOn` | Name of the previous Backup CR this incremental is built on. Empty for full backups |
| `storagePath` | Directory on the backup PVC where this backup was written |
| `startedAt` / `completedAt` | Timestamps |
| `message` | Human-readable status string |

### Restore a namespace

```yaml
apiVersion: replic2.io/v1alpha1
kind: Restore
metadata:
  name: my-app-restore-01
spec:
  namespace: my-app
  backupName: my-app-backup-01   # optional — omit to use the most recent backup
```

```bash
kubectl apply -f restore.yaml
kubectl get restores
kubectl describe restore my-app-restore-01
```

### Resource types backed up (in restore order)

1. ServiceAccounts
2. ConfigMaps
3. PersistentVolumeClaims
4. Services
5. Deployments
6. StatefulSets
7. DaemonSets
8. Ingresses

> Secrets are **not** included by default.

### Backup storage layout on PVC

```
/data/backups/
  <namespace>/
    <backup-name>/
      serviceaccounts/
        default.yaml
      configmaps/
        my-config.yaml
      deployments/
        my-app.yaml
      ...
      pvc-data/                        # only present when includePVCData: true
        <pvc-name>.tar                 # full backup archive
        <pvc-name>-incremental.tar     # incremental backup archive
```

Override the root path with the `BACKUP_ROOT` environment variable (default: `/data/backups`).

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `APP_VERSION` | `0.1.0` | Reported in `GET /` |
| `POD_NAMESPACE` | `default` | Namespace for leader election Lease |
| `POD_NAME` | OS hostname | Leader election identity |
| `BACKUP_ROOT` | `/data/backups` | PVC mount path for backup storage |
| `KUBECONFIG` | `~/.kube/config` | Kubeconfig path (local dev only) |

---

## Build & Run

```bash
# Build binary
go build -o replic2 .

# Build optimised/stripped (matches Dockerfile)
CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o replic2 .

# Run locally (falls back to ~/.kube/config; defaults to port 8080)
go run .

# Run with a custom port
PORT=9090 go run .
```

---

## Testing

```bash
# Run all tests
go test ./...

# Run with verbose output
go test -v ./...

# Run a single test
go test -v -run TestFunctionName .

# Run with race detector
go test -race ./...

# Run with coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

---

## Linting & Formatting

```bash
gofmt -w .       # format in place
go vet ./...     # static analysis
```

Always run both before committing.

---

## Deployment

Apply manifests in this order:

```bash
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/crd-backup.yaml
kubectl apply -f deploy/crd-restore.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/pvc.yaml
kubectl apply -f deploy/secret-ghcr.yaml   # edit .dockerconfigjson first
kubectl apply -f deploy/deployment.yaml
kubectl apply -f deploy/service.yaml
kubectl apply -f deploy/hpa.yaml
```

---

## Container Image

```
ghcr.io/nbebaw/replic2:latest
ghcr.io/nbebaw/replic2:sha-<short-sha>
```

Built automatically via GitHub Actions on every push to `main`.

---

## Project Structure

```
replic2/
├── main.go                                 — entry point; wires clients, HTTP server, leader election
├── main_test.go                            — HTTP handler integration tests
└── internal/
    ├── k8s/client.go                       — Kubernetes client initialisation
    ├── types/types.go                      — CRD Go types + scheme registration
    ├── store/store.go                      — PVC file I/O helpers
    ├── leader/leader.go                    — Lease-based leader election
    ├── server/
    │   ├── server.go                       — HTTP router (gin); route registration
    │   └── handler/
    │       ├── types.go                    — Response structs
    │       ├── general.go                  — Shared CR listing logic
    │       ├── backupApi.go                — /backup handler
    │       ├── restoreApi.go               — /restore handler
    │       ├── healthzApi.go               — /healthz handler
    │       └── readyzApi.go                — /readyz handler
    └── controller/
        ├── backup/backup.go                — Backup controller
        ├── restore/restore.go              — Restore controller
        └── scheduled/scheduled.go         — ScheduledBackup controller
```
