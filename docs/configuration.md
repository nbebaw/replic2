---
layout: default
title: Configuration
nav_order: 4
---

# Configuration

## Environment variables

All configuration is passed via environment variables. When deploying with Helm, these are set automatically from the S3 Secret and the Deployment spec.

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `APP_VERSION` | `0.2.0` | Version string reported by `GET /` |
| `POD_NAMESPACE` | `default` | Namespace used for leader election Lease |
| `POD_NAME` | OS hostname | Leader election identity — set to the pod name via `fieldRef` |
| `S3_ENDPOINT` | `""` | S3 endpoint URL — leave empty for AWS; set for MinIO (e.g. `http://minio:9000`) |
| `S3_BUCKET` | `""` | S3 bucket name **(required)** |
| `S3_REGION` | `us-east-1` | AWS region — any string works for MinIO |
| `S3_ACCESS_KEY_ID` | `""` | S3 access key ID **(required)** |
| `S3_SECRET_ACCESS_KEY` | `""` | S3 secret access key **(required)** |
| `S3_USE_PATH_STYLE` | `false` | Path-style S3 URLs — required for MinIO, set to `true` |
| `KUBECONFIG` | `~/.kube/config` | Kubeconfig path — used for local development only; ignored in-cluster |

---

## Deploy with raw manifests

If you are not using Helm, apply the manifests in `deploy/` in this order:

```bash
# 1. Namespace
kubectl apply -f deploy/namespace.yaml

# 2. CRDs
kubectl apply -f deploy/crd-backup.yaml
kubectl apply -f deploy/crd-restore.yaml
kubectl apply -f deploy/crd-scheduledbackup.yaml

# 3. RBAC
kubectl apply -f deploy/rbac.yaml

# 4. ServiceAccount
kubectl apply -f deploy/serviceaccount.yaml

# 5. S3 credentials secret (edit values first)
kubectl apply -f deploy/secret-s3.yaml

# 6. Image pull secret (edit .dockerconfigjson first)
kubectl apply -f deploy/secret-ghcr.yaml

# 7. Deployment
kubectl apply -f deploy/deployment.yaml

# 8. Service and HPA
kubectl apply -f deploy/service.yaml
kubectl apply -f deploy/hpa.yaml
```

> `deploy/` is gitignored — manifests live locally only and are not committed to the repository.

---

## MinIO (local / kind development)

For local development with a kind cluster, you can deploy MinIO in-cluster:

```bash
kubectl apply -f deploy/minio.yaml
```

This creates:
- A `minio` namespace with a MinIO deployment and service
- MinIO S3 API accessible at `http://minio.minio.svc.cluster.local:9000` inside the cluster
- MinIO console exposed on NodePort `30090`

Default credentials: `accessKeyId=replic2`, `secretAccessKey=replic2secret`.

Create the bucket before running a backup:

```bash
kubectl run minio-init --rm -it --restart=Never --image=amazon/aws-cli \
  --overrides='{"spec":{"containers":[{"name":"minio-init","image":"amazon/aws-cli","command":["aws","s3","mb","s3://replic2-backups","--endpoint-url","http://minio.minio.svc.cluster.local:9000"],"env":[{"name":"AWS_ACCESS_KEY_ID","value":"replic2"},{"name":"AWS_SECRET_ACCESS_KEY","value":"replic2secret"},{"name":"AWS_DEFAULT_REGION","value":"us-east-1"}]}]}}'
```

Then configure replic2 with:

```bash
--set s3.endpoint=http://minio.minio.svc.cluster.local:9000
--set s3.bucket=replic2-backups
--set s3.accessKeyId=replic2
--set s3.secretAccessKey=replic2secret
--set s3.usePathStyle=true
```
