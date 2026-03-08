---
layout: default
title: Deploy with Helm
nav_order: 2
---

# Deploy with Helm

## Install

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

## Uninstall

```bash
helm uninstall replic2 --namespace replic2
```

## Generate the image pull secret

```bash
kubectl create secret docker-registry ghcr-pull-secret \
  --docker-server=ghcr.io \
  --docker-username=<GITHUB_USER> \
  --docker-password=<GITHUB_PAT> \
  --dry-run=client -o jsonpath='{.data.\.dockerconfigjson}'
```

Pass the output as `--set imagePullSecret.dockerconfigjson=<output>`.

---

## Values reference

### Core

| Value | Default | Description |
|---|---|---|
| `replicaCount` | `2` | Number of replic2 pods. Minimum 2 for HA leader election |
| `image.repository` | `ghcr.io/nbebaw/replic2` | Container image repository |
| `image.tag` | chart `appVersion` | Image tag — defaults to chart appVersion |
| `image.pullPolicy` | `Always` | Image pull policy |
| `imagePullSecret.create` | `true` | Create the GHCR pull secret |
| `imagePullSecret.dockerconfigjson` | `""` | base64-encoded `.dockerconfigjson` **(required)** |
| `appVersion` | `0.2.0` | Version string reported by `GET /` |
| `port` | `8080` | HTTP port the container listens on |

### S3 storage

| Value | Default | Description |
|---|---|---|
| `s3.endpoint` | `""` | S3 endpoint URL — leave empty for AWS; set for MinIO (e.g. `http://minio:9000`) |
| `s3.bucket` | `replic2-backups` | S3 bucket name **(required)** |
| `s3.region` | `us-east-1` | AWS region — any string works for MinIO |
| `s3.usePathStyle` | `false` | Path-style addressing — required for MinIO, set to `"true"` |
| `s3.accessKeyId` | `""` | S3 access key ID **(required)** |
| `s3.secretAccessKey` | `""` | S3 secret access key **(required)** |

### Service

| Value | Default | Description |
|---|---|---|
| `service.type` | `NodePort` | Service type: `ClusterIP` \| `NodePort` \| `LoadBalancer` |
| `service.port` | `80` | Port exposed by the Service |
| `service.nodePort` | `30080` | NodePort number (only when `type=NodePort`) |
| `service.annotations` | `{}` | Annotations on the Service (e.g. cloud LB annotations) |
| `service.clusterIP` | `""` | Static ClusterIP (only when `type=ClusterIP`) |
| `service.loadBalancerIP` | `""` | Static IP for LoadBalancer services |
| `service.loadBalancerSourceRanges` | `[]` | CIDR allowlist for LoadBalancer services |

### Ingress

| Value | Default | Description |
|---|---|---|
| `ingress.enabled` | `false` | Enable an Ingress resource |
| `ingress.className` | `""` | IngressClass name (e.g. `nginx`, `traefik`, `alb`) |
| `ingress.annotations` | `{}` | Annotations on the Ingress |
| `ingress.hosts` | see values.yaml | Host rules and paths |
| `ingress.tls` | `[]` | TLS configuration |

### Autoscaling

| Value | Default | Description |
|---|---|---|
| `autoscaling.enabled` | `true` | Enable the HorizontalPodAutoscaler |
| `autoscaling.minReplicas` | `2` | HPA minimum replicas |
| `autoscaling.maxReplicas` | `10` | HPA maximum replicas |
| `autoscaling.targetCPUUtilizationPercentage` | `70` | HPA CPU target (%) |
| `autoscaling.targetMemoryUtilizationPercentage` | `80` | HPA memory target (%) |

### Availability and scheduling

| Value | Default | Description |
|---|---|---|
| `podDisruptionBudget.enabled` | `true` | Enable PodDisruptionBudget |
| `podDisruptionBudget.minAvailable` | `1` | Minimum pods available during voluntary disruptions |
| `podDisruptionBudget.maxUnavailable` | `""` | Maximum unavailable pods (mutually exclusive with `minAvailable`) |
| `serviceAccount.annotations` | `{}` | ServiceAccount annotations — e.g. AWS IRSA role ARN |
| `podAnnotations` | `{}` | Annotations added to every Pod |
| `nodeSelector` | `{}` | Node selector for Pod scheduling |
| `tolerations` | `[]` | Tolerations for Pod scheduling |
| `affinity` | `{}` | Affinity / anti-affinity rules |
| `topologySpreadConstraints` | `[]` | Topology spread constraints (Kubernetes 1.19+) |

### Resources

| Value | Default | Description |
|---|---|---|
| `resources.requests.cpu` | `50m` | CPU request |
| `resources.requests.memory` | `64Mi` | Memory request |
| `resources.limits.cpu` | `200m` | CPU limit |
| `resources.limits.memory` | `128Mi` | Memory limit |

---

## Ingress examples

### nginx ingress controller (CLI)

```bash
helm upgrade --install replic2 charts/replic2 \
  --set imagePullSecret.dockerconfigjson=<base64> \
  --set ingress.enabled=true \
  --set ingress.className=nginx \
  --set "ingress.hosts[0].host=replic2.example.com" \
  --set "ingress.hosts[0].paths[0].path=/" \
  --set "ingress.tls[0].secretName=replic2-tls" \
  --set "ingress.tls[0].hosts[0]=replic2.example.com"
```

### nginx + cert-manager (values file)

```yaml
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

### AWS ALB ingress controller

```yaml
ingress:
  enabled: true
  className: alb
  annotations:
    alb.ingress.kubernetes.io/scheme: internet-facing
    alb.ingress.kubernetes.io/target-type: ip
  hosts:
    - host: replic2.example.com
      paths:
        - path: /
          pathType: Prefix
```

---

## AWS IRSA (IAM Roles for Service Accounts)

If running on EKS and using AWS S3, you can avoid storing credentials in a Secret by using IRSA:

```yaml
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/replic2-s3-role

s3:
  bucket: my-replic2-backups
  region: us-east-1
  accessKeyId: ""
  secretAccessKey: ""
```

The IAM role must have `s3:GetObject`, `s3:PutObject`, `s3:ListBucket`, and `s3:DeleteObject` on the bucket.

---

## Lint and dry-run

```bash
# Lint
helm lint charts/replic2 --set imagePullSecret.dockerconfigjson=dGVzdA==

# Dry-run render
helm template replic2 charts/replic2 \
  --namespace replic2 \
  --set imagePullSecret.dockerconfigjson=dGVzdA==
```
