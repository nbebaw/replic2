---
layout: default
title: Home
nav_order: 1
---

# replic2

A Kubernetes operator and HTTP server that provides namespace-scoped backup and restore via Custom Resources, with S3-compatible object storage as the only backend.

## Features

- **Backup controller** — watches `Backup` CRs; writes namespace manifests directly to S3 as YAML objects; optionally copies raw PVC data to S3 via a temporary agent pod; auto-selects Full or Incremental backup type.
- **Restore controller** — watches `Restore` CRs; downloads manifests from S3 and re-applies them via Server-Side Apply; restores raw PVC data by extracting S3 tar archives into freshly provisioned PVCs.
- **ScheduledBackup controller** — cron-based automatic backup creation.
- **Leader election** — Lease-based; only the elected pod runs controllers. Standby pods still serve HTTP.
- **HTTP API** — exposes metadata, health probes, and CR listings.

---

## Quick start

### 1. Install with Helm

```bash
helm upgrade --install replic2 charts/replic2 \
  --namespace replic2 --create-namespace \
  --set imagePullSecret.dockerconfigjson=<base64> \
  --set s3.endpoint=http://minio:9000 \
  --set s3.bucket=replic2-backups \
  --set s3.accessKeyId=<key> \
  --set s3.secretAccessKey=<secret> \
  --set s3.usePathStyle=true
```

See [Deploy with Helm](./helm) for the full values reference and ingress setup.

### 2. Back up a namespace

```yaml
apiVersion: replic2.io/v1alpha1
kind: Backup
metadata:
  name: my-app-backup-01
spec:
  namespace: my-app
  includePVCData: true
  ttl: 168h
```

```bash
kubectl apply -f backup.yaml
kubectl get backups
```

### 3. Restore a namespace

```yaml
apiVersion: replic2.io/v1alpha1
kind: Restore
metadata:
  name: my-app-restore-01
spec:
  namespace: my-app
  backupName: my-app-backup-01
```

```bash
kubectl apply -f restore.yaml
kubectl get restores
```

See [Custom Resources](./custom-resources) for all spec fields and status fields.

---

## HTTP endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/` | Application metadata (version, hostname, namespace, timestamp) |
| `GET` | `/healthz` | Liveness probe |
| `GET` | `/readyz` | Readiness probe |
| `GET` | `/backup` | List all `Backup` CRs (name, phase, completedAt) |
| `GET` | `/restore` | List all `Restore` CRs (name, phase, completedAt) |

---

## Container image

```
ghcr.io/nbebaw/replic2:latest
ghcr.io/nbebaw/replic2:sha-<short-sha>
```

Built automatically via GitHub Actions on every push to `main`.
