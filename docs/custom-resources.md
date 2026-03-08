---
layout: default
title: Custom Resources
nav_order: 3
---

# Custom Resources

replic2 defines three CRDs under the `replic2.io/v1alpha1` API group.

---

## Backup

The `Backup` CR triggers a one-shot backup of a namespace to S3.

### Spec fields

| Field | Type | Default | Description |
|---|---|---|---|
| `namespace` | string | required | Namespace to back up |
| `type` | string | auto | `Full` or `Incremental`. Omit to auto-select: Full on first run, Incremental after |
| `includePVCData` | bool | `false` | Also copy raw data from every bound PVC in the namespace |
| `ttl` | string | — | Go duration (e.g. `24h`, `168h`). CR and S3 data are deleted after `completedAt + ttl` |

### Status fields

| Field | Description |
|---|---|
| `phase` | `Pending` → `InProgress` → `Completed` \| `Failed` |
| `backupType` | `Full` or `Incremental` — what was actually performed |
| `basedOn` | Name of the previous Backup CR this incremental is built on. Empty for full backups |
| `storagePath` | S3 key prefix for this backup (e.g. `my-app/my-backup-01`) |
| `startedAt` / `completedAt` | RFC3339 timestamps |
| `message` | Human-readable status string |

### Examples

```yaml
# Minimal — manifests only, type auto-selected
apiVersion: replic2.io/v1alpha1
kind: Backup
metadata:
  name: my-app-backup-01
spec:
  namespace: my-app
```

```yaml
# Full backup with PVC data and a 7-day TTL
apiVersion: replic2.io/v1alpha1
kind: Backup
metadata:
  name: my-app-backup-full
spec:
  namespace: my-app
  type: Full
  includePVCData: true
  ttl: 168h
```

```yaml
# Incremental backup — only manifests and PVC files changed since the last backup
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

---

## Restore

The `Restore` CR restores a namespace from a completed backup.

The controller:
1. Re-creates the namespace if it was deleted.
2. Re-applies all backed-up manifests via Server-Side Apply (dependency-ordered: ServiceAccounts → ConfigMaps → PVCs → Services → Deployments → StatefulSets → DaemonSets → Ingresses).
3. Waits for each PVC to become `Bound`, then restores raw data from S3 into the new PVC.

> Only **full** backup archives are used for PVC data restore. Incremental archives cannot be extracted stand-alone.

### Spec fields

| Field | Type | Default | Description |
|---|---|---|---|
| `namespace` | string | required | Namespace to restore into (created if absent) |
| `backupName` | string | — | Name of the `Backup` CR to restore from. Omit to use the most recent completed backup for the namespace |

### Status fields

| Field | Description |
|---|---|
| `phase` | `Pending` → `InProgress` → `Completed` \| `Failed` |
| `restoredFrom` | S3 key prefix of the backup that was used |
| `startedAt` / `completedAt` | RFC3339 timestamps |
| `message` | Human-readable status string |

### Examples

```yaml
# Restore from a specific backup
apiVersion: replic2.io/v1alpha1
kind: Restore
metadata:
  name: my-app-restore-01
spec:
  namespace: my-app
  backupName: my-app-backup-01
```

```yaml
# Restore from the most recent completed backup
apiVersion: replic2.io/v1alpha1
kind: Restore
metadata:
  name: my-app-restore-latest
spec:
  namespace: my-app
```

```bash
kubectl apply -f restore.yaml
kubectl get restores
kubectl describe restore my-app-restore-01
```

---

## ScheduledBackup

The `ScheduledBackup` CR creates `Backup` CRs automatically on a cron schedule.

### Spec fields

| Field | Type | Default | Description |
|---|---|---|---|
| `namespace` | string | required | Namespace to back up on each trigger |
| `schedule` | string | required | Cron expression (e.g. `0 2 * * *` for 02:00 daily) |
| `includePVCData` | bool | `false` | Passed through to each created `Backup` CR |
| `ttl` | string | — | Passed through to each created `Backup` CR |

### Example

```yaml
apiVersion: replic2.io/v1alpha1
kind: ScheduledBackup
metadata:
  name: my-app-daily
spec:
  namespace: my-app
  schedule: "0 2 * * *"   # every day at 02:00 UTC
  includePVCData: true
  ttl: 168h               # keep each backup for 7 days
```

```bash
kubectl apply -f scheduledbackup.yaml
kubectl get scheduledbackups
```

---

## Resource types backed up

Resources are backed up and restored in this order:

1. ServiceAccounts
2. ConfigMaps
3. PersistentVolumeClaims
4. Services
5. Deployments
6. StatefulSets
7. DaemonSets
8. Ingresses

> **Secrets are not backed up by default.**

---

## S3 storage layout

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
