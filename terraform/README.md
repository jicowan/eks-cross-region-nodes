# Terraform — EKS Cross-Region Nodes (Same-Account)

Provisions the full infrastructure for a same-account / cross-region EKS satellite node deployment.

## Architecture

```
┌─────────────── us-west-2 ───────────────┐     ┌─────────────── us-east-1 ───────────────┐
│  VPC 10.50.0.0/16                       │     │  VPC 10.60.0.0/16                       │
│  ┌──────────────────────────────────┐   │     │  ┌──────────────────────────────────┐   │
│  │  EKS Cluster (xrn-demo)         │   │     │  │  Satellite ASG                    │   │
│  │  + Managed Node Group (2 nodes) │   │     │  │  + xrn-install bootstrap          │   │
│  └──────────────────────────────────┘   │     │  └──────────────────────────────────┘   │
│          │                              │     │          │                              │
│       [TGW] ─────── peering ──────────────────────── [TGW]                             │
└─────────────────────────────────────────┘     └─────────────────────────────────────────┘
```

## Prerequisites

- AWS CLI configured with credentials for the target account
- Terraform >= 1.5
- Go >= 1.26 (to build `xrnctl` from the repo root)

## Usage

```bash
# 1. Configure
cp terraform.tfvars.example terraform.tfvars
# Edit terraform.tfvars with your values

# 2. Deploy infrastructure
terraform init
terraform plan -out=tfplan.out
terraform apply tfplan.out

# 3. Update kubeconfig
aws eks update-kubeconfig --region us-west-2 --name xrn-demo

# 4. Register satellite (post-apply — sets RemoteNetworkConfig + ConfigMap)
cd .. && make build  # builds xrnctl
./xrnctl add-satellite \
  --cluster-name xrn-demo --cluster-region us-west-2 \
  --vpc-id <satellite_vpc_id from output> --satellite-region us-east-1

# 5. Approve CSRs (after satellite nodes register)
kubectl get csr -o name | xargs kubectl certificate approve

# 6. Verify
./xrnctl verify --cluster-name xrn-demo --cluster-region us-west-2

# 7. Wait ~15-30 min for RemoteNetworkConfig propagation, then test:
kubectl exec <pod-on-satellite> -- hostname
```

## Modules

| Module | Purpose |
|--------|---------|
| `modules/vpc` | Reusable VPC (public + private subnets, NAT, IGW) |
| `modules/eks` | Standard EKS cluster with satellite SG rules |
| `modules/local-nodes` | Managed node group for cluster-VPC workloads |
| `modules/transit-gateway` | TGW in both regions + peering + route injection |
| `modules/iam` | CrossRegionNodeRole + instance profile + HYBRID_LINUX access entry |
| `modules/satellite-asg` | Launch template + ASG + user data template |

## Important Notes

- **Do NOT use EKS Auto Mode** — cross-region satellite nodes require standard mode.
- **`RemoteNetworkConfig` is managed by `xrnctl`**, not Terraform. The EKS cluster resource has `lifecycle { ignore_changes = [vpc_config] }` to prevent Terraform from overwriting it.
- **Run `xrnctl add-satellite` before scaling the ASG** for the cleanest bootstrap experience.
- **CSR auto-approval** is not provided by EKS for cross-region nodes. Install a CSR approver (e.g. kubelet-rubber-stamp) or approve manually.

## Destroy

```bash
terraform destroy
```
