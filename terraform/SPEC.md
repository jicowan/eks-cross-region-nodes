# Terraform Spec — EKS Cross-Region Nodes (Same-Account)

## Overview

This Terraform project provisions two fresh environments:

- **Cluster region** (`us-west-2`): New VPC + EKS cluster (the "home" cluster)
- **Satellite region** (configurable, e.g. `us-east-1`): New VPC + ASG with cross-region worker nodes

A Transit Gateway with cross-region peering connects the two VPCs.

---

## Account & Assumptions

| Item | Value |
|------|-------|
| AWS Account | `259949759491` |
| IAM User | `hurley` |
| Cluster Region | `us-west-2` |
| Satellite Region | variable (default `us-east-1`) |
| Topology | Same-account / cross-region |
| Cluster Mode | **Standard** (not Auto Mode — see note below) |
| Node bootstrap tool | `xrn-install` (from GitHub releases) |

> **Why not EKS Auto Mode?** Auto Mode uses AWS-managed Karpenter NodePools to provision
> compute. Cross-region satellite nodes are inherently self-managed EC2 instances that
> register via `HYBRID_LINUX` access entries and custom user-data (`xrn-install`). Auto Mode
> cannot launch nodes in a remote VPC/region, and its managed VPC CNI configuration is not
> compatible with the `aws-node-vpc-cidrs` ConfigMap + `RemoteNetworkConfig` changes required
> for cross-region networking. Additionally, the EC2 Instance Store CSI driver (recommended
> for satellite node scratch storage) is not available on Auto Mode. The cluster is therefore
> provisioned as a standard EKS cluster with `authentication_mode = "API"`.
>
> A small **local managed node group** (2 nodes in the cluster VPC) is included for
> control-plane-adjacent workloads (CoreDNS, kube-proxy, cluster-autoscaler, CSR approver).

---

## Module Layout

```
terraform/
├── SPEC.md                  ← this file
├── main.tf                  ← root module, provider configs, locals
├── variables.tf             ← input variables
├── outputs.tf               ← key outputs (cluster endpoint, node role ARN, etc.)
├── providers.tf             ← provider aliases (cluster region + satellite region)
├── versions.tf              ← terraform/provider version constraints
│
├── modules/
│   ├── vpc/                 ← reusable VPC module (used twice: cluster + satellite)
│   │   ├── main.tf
│   │   ├── variables.tf
│   │   └── outputs.tf
│   │
│   ├── eks/                 ← EKS cluster (cluster region only)
│   │   ├── main.tf
│   │   ├── variables.tf
│   │   └── outputs.tf
│   │
│   ├── local-nodes/         ← small managed node group in cluster VPC
│   │   ├── main.tf
│   │   ├── variables.tf
│   │   └── outputs.tf
│   │
│   ├── transit-gateway/     ← TGW + cross-region peering + route propagation
│   │   ├── main.tf
│   │   ├── variables.tf
│   │   └── outputs.tf
│   │
│   ├── iam/                 ← Node IAM role, instance profile, access entry
│   │   ├── main.tf
│   │   ├── variables.tf
│   │   └── outputs.tf
│   │
│   └── satellite-asg/       ← Launch template + ASG in satellite region
│       ├── main.tf
│       ├── variables.tf
│       ├── outputs.tf
│       └── userdata.tftpl   ← templatefile() version of userdata.template.txt
```

---

## Variables (root)

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `cluster_name` | string | `"xrn-demo"` | EKS cluster name |
| `cluster_region` | string | `"us-west-2"` | Region for the EKS cluster |
| `satellite_region` | string | `"us-east-1"` | Region for satellite worker nodes |
| `cluster_vpc_cidr` | string | `"10.50.0.0/16"` | CIDR for the cluster VPC |
| `satellite_vpc_cidr` | string | `"10.60.0.0/16"` | CIDR for the satellite VPC |
| `kubernetes_version` | string | `"1.32"` | EKS Kubernetes version |
| `node_instance_type` | string | `"m6i.large"` | Instance type for satellite nodes |
| `local_node_instance_type` | string | `"m6i.medium"` | Instance type for local managed node group |
| `local_node_count` | number | `2` | Desired count for local managed node group |
| `asg_desired_capacity` | number | `1` | Desired satellite node count |
| `asg_max_size` | number | `3` | Max satellite node count |
| `xrn_install_url` | string | `"https://github.com/jicowan/eks-cross-region-nodes/releases/latest/download/xrn-install-linux-amd64"` | Download URL for xrn-install binary |

---

## Module Details

### 1. `modules/vpc`

Creates a VPC with public + private subnets across 2 AZs. Used for both regions.

| Resource | Purpose |
|----------|---------|
| `aws_vpc` | VPC with specified CIDR |
| `aws_subnet` (×4) | 2 public + 2 private subnets |
| `aws_internet_gateway` | Outbound for public subnets |
| `aws_nat_gateway` | Outbound for private subnets (satellite nodes need to pull `xrn-install`) |
| `aws_route_table` + associations | Routing for public/private |
| `aws_eip` | NAT Gateway elastic IP |

**Outputs:** `vpc_id`, `vpc_cidr`, `private_subnet_ids`, `public_subnet_ids`, `private_route_table_ids`

---

### 2. `modules/eks`

Provisions a standard EKS cluster (not Auto Mode) with `authentication_mode = "API"`.

| Resource | Purpose |
|----------|---------|
| `aws_eks_cluster` | Standard cluster with `authentication_mode = "API"` (NOT Auto Mode) |
| `aws_iam_role` (cluster role) | EKS service role |
| `aws_iam_role_policy_attachment` | `AmazonEKSClusterPolicy` |
| `aws_security_group` | Cluster SG with cross-region inbound rules |
| `aws_security_group_rule` | Inbound from satellite VPC CIDR: TCP 443, 53, 10250, 1024-65535 |

**Outputs:** `cluster_name`, `cluster_endpoint`, `cluster_ca`, `cluster_service_cidr`, `cluster_security_group_id`, `cluster_version`

---

### 3. `modules/local-nodes`

Small managed node group in the cluster VPC for control-plane-adjacent workloads.

| Resource | Purpose |
|----------|---------|
| `aws_eks_node_group` | Managed node group (2 nodes, private subnets) |
| `aws_iam_role` | Node group IAM role (standard EKS worker policies) |
| `aws_iam_role_policy_attachment` (×3) | `AmazonEKSWorkerNodePolicy`, `AmazonEC2ContainerRegistryReadOnly`, `AmazonEKS_CNI_Policy` |

These nodes run CoreDNS, kube-proxy, cluster-autoscaler, and the CSR auto-approver.
Satellite pods communicate with CoreDNS on these nodes via the TGW.

**Outputs:** `node_group_name`, `node_group_role_arn`

---

### 4. `modules/transit-gateway`

Cross-region connectivity via Transit Gateway peering.

| Resource | Purpose |
|----------|---------|
| `aws_ec2_transit_gateway` (cluster region) | TGW in us-west-2 |
| `aws_ec2_transit_gateway` (satellite region) | TGW in satellite region |
| `aws_ec2_transit_gateway_vpc_attachment` (×2) | Attach each VPC to its regional TGW |
| `aws_ec2_transit_gateway_peering_attachment` | Cross-region TGW peering |
| `aws_ec2_transit_gateway_peering_attachment_accepter` | Accept the peering |
| `aws_ec2_transit_gateway_route` (×2) | Routes: cluster→satellite CIDR and satellite→cluster CIDR |
| `aws_route` (×N) | VPC route table entries pointing satellite CIDR → TGW (and vice versa) |

**Outputs:** `cluster_tgw_id`, `satellite_tgw_id`, `peering_attachment_id`

---

### 5. `modules/iam`

Node IAM role + instance profile + EKS access entry.

| Resource | Purpose |
|----------|---------|
| `aws_iam_role` (`CrossRegionNodeRole`) | EC2 trust, worker node policies |
| `aws_iam_role_policy_attachment` (×4) | `AmazonEKSWorkerNodePolicy`, `AmazonEC2ContainerRegistryReadOnly`, `AmazonEKS_CNI_Policy`, `AmazonSSMManagedInstanceCore` |
| `aws_iam_role_policy` | Inline: `eks:ListAccessEntries`, `eks:DescribeAccessEntry` |
| `aws_iam_instance_profile` | For launch template |
| `aws_eks_access_entry` | Type `HYBRID_LINUX`, principal = node role ARN |

**Outputs:** `node_role_arn`, `instance_profile_name`

---

### 6. `modules/satellite-asg`

Launch template + ASG in the satellite region.

| Resource | Purpose |
|----------|---------|
| `aws_launch_template` | AL2023 EKS AMI, instance profile, SG, user data (from `userdata.tftpl`) |
| `aws_autoscaling_group` | In private subnets of satellite VPC, CA discovery tags |
| `aws_security_group` (satellite) | Inbound from cluster VPC CIDR: TCP 1024-65535, 10250 |
| `aws_ssm_parameter` (data) | Look up latest AL2023 EKS-optimized AMI |

**User data** (`userdata.tftpl`): MIME multipart template matching the repo's `deploy/asg/userdata.template.txt`, rendered via `templatefile()` with cluster endpoint, CA, service CIDR, and xrn-install URL.

**Outputs:** `asg_name`, `launch_template_id`, `satellite_security_group_id`

---

## What Remains Outside Terraform (run via `xrnctl`)

These steps are performed after `terraform apply`:

```bash
# 1. Register the satellite (updates ConfigMap + RemoteNetworkConfig)
xrnctl add-satellite \
  --cluster-name xrn-demo --cluster-region us-west-2 \
  --vpc-id <satellite_vpc_id from TF output> --satellite-region us-east-1

# 2. Verify
xrnctl verify --cluster-name xrn-demo --cluster-region us-west-2
```

Optionally wrap these in a `null_resource` with `local-exec` at the end of the root module.

---

## Dependency Graph

```
vpc (cluster) ─────┐                              
                    ├──→ eks (standard, not Auto Mode)
                    │       ├──→ local-nodes (managed node group)
                    │       └──→ iam ──→ access_entry
vpc (satellite) ───┤                         │    
                    ├──→ transit-gateway      │    
                    │         │               │    
                    │         ▼               ▼    
                    └──→ satellite-asg (needs: ami, subnets, SG, instance profile,
                                              cluster endpoint/CA/service_cidr)
```

---

## Next Steps

1. Confirm CIDRs (`10.50.0.0/16` and `10.60.0.0/16`) don't conflict with existing VPCs
2. Confirm satellite region choice (`us-east-1`)
3. Generate the actual `.tf` files
