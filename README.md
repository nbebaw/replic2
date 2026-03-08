# replic2

A Kubernetes operator and HTTP server that provides namespace-scoped backup and restore via Custom Resources, with S3-compatible object storage as the only backend.

## Features

- **Backup controller** — watches `Backup` CRs; writes namespace manifests directly to S3 as YAML objects; optionally copies raw PVC data to S3 via a temporary agent pod running `amazon/aws-cli`; auto-selects Full or Incremental backup type.
- **Restore controller** — watches `Restore` CRs; downloads manifests from S3 and re-applies them via Server-Side Apply; restores raw PVC data by extracting S3 tar archives into freshly provisioned PVCs.
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

## Deploy with Helm

```bash
# Install into the replic2 namespace (creates it if absent)
helm upgrade --install replic2 charts/replic2 \
  --namespace replic2 --create-namespace \
  --set imagePullSecret.dockerconfigjson=<base64> \
  --set s3.endpoint=http://minio:9000 \
  --set s3.bucket=replic2-backups \
  --set s3.accessKeyId=<key> \
  --set s3.secretAccessKey=<secret> \
  --set s3.usePathStyle=true

# Uninstall
helm uninstall replic2 --namespace replic2
```

Generate the base64 image pull secret value with:

```bash
kubectl create secret docker-registry ghcr-pull-secret \
  --docker-server=ghcr.io \
  --docker-username=<GITHUB_USER> \
  --docker-password=<GITHUB_PAT> \
  --dry-run=client -o jsonpath='{.data.\.dockerconfigjson}'
```

### Helm values reference

| Value | Default | Description |
|---|---|---|
| `replicaCount` | `2` | Number of replic2 pods |
| `image.repository` | `ghcr.io/nbebaw/replic2` | Container image |
| `image.tag` | chart `appVersion` | Image tag (defaults to chart appVersion) |
| `image.pullPolicy` | `Always` | Image pull policy |
| `imagePullSecret.create` | `true` | Create the GHCR pull secret from the value below |
| `imagePullSecret.dockerconfigjson` | `""` | base64-encoded .dockerconfigjson **(required)** |
| `s3.endpoint` | `""` | S3 endpoint URL — leave empty for AWS; set for MinIO |
| `s3.bucket` | `replic2-backups` | S3 bucket name **(required)** |
| `s3.region` | `us-east-1` | AWS region (any string for MinIO) |
| `s3.usePathStyle` | `false` | Path-style addressing — set `true` for MinIO |
| `s3.accessKeyId` | `""` | S3 access key ID **(required)** |
| `s3.secretAccessKey` | `""` | S3 secret access key **(required)** |
| `service.type` | `NodePort` | Service type: `ClusterIP` \| `NodePort` \| `LoadBalancer` |
| `service.port` | `80` | Service port |
| `service.nodePort` | `30080` | NodePort number (only when `type=NodePort`) |
| `service.annotations` | `{}` | Annotations on the Service (e.g. cloud LB annotations) |
| `service.loadBalancerIP` | `""` | Static IP for LoadBalancer services |
| `service.loadBalancerSourceRanges` | `[]` | CIDR allowlist for LoadBalancer services |
| `ingress.enabled` | `false` | Enable an Ingress resource |
| `ingress.className` | `""` | IngressClass name (e.g. `nginx`, `traefik`, `alb`) |
| `ingress.annotations` | `{}` | Annotations on the Ingress |
| `ingress.hosts` | see values.yaml | Host rules and paths |
| `ingress.tls` | `[]` | TLS configuration |
| `autoscaling.enabled` | `true` | Enable HPA |
| `autoscaling.minReplicas` | `2` | HPA minimum replicas |
| `autoscaling.maxReplicas` | `10` | HPA maximum replicas |
| `autoscaling.targetCPUUtilizationPercentage` | `70` | HPA CPU target |
| `autoscaling.targetMemoryUtilizationPercentage` | `80` | HPA memory target |
| `podDisruptionBudget.enabled` | `true` | Enable PodDisruptionBudget |
| `podDisruptionBudget.minAvailable` | `1` | Minimum pods available during disruptions |
| `serviceAccount.annotations` | `{}` | ServiceAccount annotations (e.g. AWS IRSA role ARN) |
| `podAnnotations` | `{}` | Annotations added to every Pod |
| `nodeSelector` | `{}` | Node selector for Pod scheduling |
| `tolerations` | `[]` | Tolerations for Pod scheduling |
| `affinity` | `{}` | Affinity rules for Pod scheduling |
| `topologySpreadConstraints` | `[]` | Topology spread constraints (Kubernetes 1.19+) |
| `resources.requests.cpu` | `50m` | CPU request |
| `resources.requests.memory` | `64Mi` | Memory request |
| `resources.limits.cpu` | `200m` | CPU limit |
| `resources.limits.memory` | `128Mi` | Memory limit |

### Ingress examples

```bash
# nginx ingress controller
helm upgrade --install replic2 charts/replic2 \
  --set imagePullSecret.dockerconfigjson=<base64> \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set "ingress.hosts[0].host=replic2.example.com" \
  --set "ingress.hosts[0].paths[0].path=/" \
  --set "ingress.tls[0].secretName=replic2-tls" \
  --set "ingress.tls[0].hosts[0]=replic2.example.com"
```

```yaml
# values override file — full ingress example with cert-manager
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/ssl-redirect: "true"
  hosts:
    - host: replic2.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: replic2-tls
      hosts:
        - replic2.example.com
```

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
| `ttl` | string | — | Go duration (e.g. `24h`). CR and S3 data are deleted after `completedAt + ttl` |

#### Backup status fields written by the controller

| Field | Description |
|---|---|
| `phase` | `Pending` → `InProgress` → `Completed` \| `Failed` |
| `backupType` | `Full` or `Incremental` — what was actually performed |
| `basedOn` | Name of the previous Backup CR this incremental is built on. Empty for full backups |
| `storagePath` | S3 key prefix for this backup (e.g. `my-app/my-backup-01`) |
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

The restore controller:
1. Re-creates the namespace if it was deleted.
2. Re-applies all backed-up manifests via Server-Side Apply (dependency-ordered).
3. Waits for each PVC to become `Bound`, then restores raw data from S3 into the new PVC.

> Only full backup archives are used for PVC data restore. Incremental archives cannot be extracted stand-alone.

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

### Backup storage layout on S3

```
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

---

## Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `APP_VERSION` | `0.2.0` | Reported in `GET /` |
| `POD_NAMESPACE` | `default` | Namespace for leader election Lease |
| `POD_NAME` | OS hostname | Leader election identity |
| `S3_ENDPOINT` | `""` | S3 endpoint URL (empty = AWS; set for MinIO) |
| `S3_BUCKET` | `""` | S3 bucket name **(required)** |
| `S3_REGION` | `us-east-1` | AWS region |
| `S3_ACCESS_KEY_ID` | `""` | S3 access key ID **(required)** |
| `S3_SECRET_ACCESS_KEY` | `""` | S3 secret access key **(required)** |
| `S3_USE_PATH_STYLE` | `false` | Path-style S3 URLs — required for MinIO |
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

## Deploy with raw manifests

Apply in this order if not using Helm:

```bash
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/crd-backup.yaml
kubectl apply -f deploy/crd-restore.yaml
kubectl apply -f deploy/crd-scheduledbackup.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/secret-s3.yaml       # edit S3 credentials first
kubectl apply -f deploy/secret-ghcr.yaml     # edit .dockerconfigjson first
kubectl apply -f deploy/deployment.yaml
kubectl apply -f deploy/service.yaml
kubectl apply -f deploy/hpa.yaml
```

> `deploy/` is gitignored — manifests live locally only.

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
├── charts/replic2/                         — Helm chart
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
    ├── k8s/client.go                       — Kubernetes client initialisation
    ├── s3/client.go                        — S3 client init from env vars
    ├── types/types.go                      — CRD Go types + scheme registration
    ├── store/store.go                      — S3 I/O helpers (PutObject, GetObject, ListKeys, DeletePrefix)
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
        ├── backup/
        │   ├── backup.go                   — poll loop, constants, exported wrappers
        │   ├── process.go                  — process(), FindLatestCompletedBackup()
        │   ├── manifests.go                — resource type discovery, S3 PutObject for manifests
        │   ├── pvcdata.go                  — agent pod that tars PVC data and streams to S3
        │   └── status.go                   — status patching, TTL expiry, S3 DeletePrefix
        ├── restore/
        │   └── restore.go                  — manifest restore from S3; PVC data restore via agent pod
        └── scheduled/scheduled.go         — ScheduledBackup controller
```
