# eks-cross-region-nodes

Tools for joining EC2 worker nodes in satellite VPCs/regions to an EKS cluster in a different region.

## Components

### `xrn-install` (Phase 3)

On-node installer that bootstraps a satellite EC2 instance into a cross-region EKS cluster. Replaces the manual user-data fixup documented in the Phase 1 runbook.

**What it does:**
1. Discovers cluster configuration via `eks:DescribeCluster` (cross-region)
2. Discovers node metadata from IMDS
3. Runs pre-flight checks (API endpoint reachable, DNS resolves, regions differ)
4. Invokes `nodeadm init` with discovered config
5. Patches kubelet configuration for cross-region operation:
   - `--cloud-provider=""` (prevents CCM from deleting the node)
   - `--hostname-override=<instance-id>` (matches HYBRID_LINUX identity)
   - `providerID=eks-hybrid:///<cluster-region>/<cluster-name>/<instance-id>`
   - Topology labels via `--node-labels`
   - Kubeconfig region set to cluster region (for valid STS token)
6. Restarts kubelet

**Usage:**
```bash
xrn-install init --cluster-name my-cluster --cluster-region us-east-2
```

**Prerequisites:**
- EC2 instance with an IAM role that has a `HYBRID_LINUX` access entry on the target cluster
- Network connectivity to the cluster API endpoint (TGW/peering + cluster SG rule)
- AL2023 EKS-optimized AMI with `nodeadm` available
- IAM role attached to the instance must have these AWS-managed policies:
  - `AmazonEKSWorkerNodePolicy`
  - `AmazonEC2ContainerRegistryReadOnly`
  - `AmazonEKS_CNI_Policy`
  - `AmazonSSMManagedInstanceCore` (optional, for debugging)
- IAM role must also allow `eks:ListAccessEntries` and `eks:DescribeAccessEntry` (for `xrn-install`'s preflight check). Add as an inline policy — see [docs/runbook-phase1.md §4](docs/runbook-phase1.md#4-iam-for-the-worker-node).
- (Only if using custom networking) ENIConfig CRs created for this node's AZ via `xrnctl add-region --with-eniconfigs`

### `xrnctl` (Phase 4)

Cluster-admin tool for managing satellite regions. Runs from an operator workstation with kubectl access to the cluster.

**What it does:**
- Maintains the `aws-node-vpc-cidrs` ConfigMap (SNAT exclusion CIDRs + registry metadata)
- Validates CIDR non-overlap when adding a new region
- Optionally creates/deletes `ENIConfig` CRs (only when custom networking is required)
- Refuses to remove a region with nodes still registered
- Detects configuration drift

**Usage:**
```bash
# Register a new satellite VPC (default: no ENIConfigs, pods use the node's subnet)
xrnctl add-region \
  --cluster-name main --cluster-region us-east-2 \
  --vpc-id vpc-09c3d15c27ab543c5 --satellite-region eu-west-1

# Or with custom networking (pods need a different subnet/SG than the node)
xrnctl add-region \
  --cluster-name main --cluster-region us-east-2 \
  --vpc-id vpc-09c3d15c27ab543c5 --satellite-region eu-west-1 \
  --with-eniconfigs \
  --subnet-ids subnet-aaa,subnet-bbb \
  --security-group-ids sg-xxx

# List all registered satellites
xrnctl list-regions --cluster-name main --cluster-region us-east-2

# Verify consistency
xrnctl verify --cluster-name main --cluster-region us-east-2

# Remove a satellite (fails if nodes still present)
xrnctl remove-region --cluster-name main --cluster-region us-east-2 --vpc-id vpc-09c3d15c27ab543c5
```

**What `add-region` does by default:**
- Updates `kube-system/aws-node-vpc-cidrs` ConfigMap:
  - `exclude-snat-cidrs` — all VPC CIDRs (cluster + all satellites), newline-separated
  - `registry.json` — full satellite metadata for verify/remove operations

**With `--with-eniconfigs`:**
- Additionally creates one `ENIConfig` per AZ in the satellite VPC (name = AZ name, e.g., `eu-west-1b`)
- Required only when pods must use a different subnet or security group than the node. For most cross-region setups, the default (no ENIConfigs) is preferred — the VPC CNI auto-discovers the node's subnet via IMDS.

**Prerequisites:**
- kubectl access to the cluster (kubeconfig configured)
- AWS credentials with `ec2:DescribeVpcs` in the satellite region and `eks:DescribeCluster` in the cluster region
- The satellite VPC must already exist with subnets and security groups created

**What `verify` checks:**
- Every satellite in `registry.json` has matching ENIConfigs in the cluster
- Every CIDR in `registry.json` is present in `exclude-snat-cidrs`
- Every node labeled `compute-type=cross-region` has an ENIConfig for its AZ

## Bootstrapping a satellite node with `xrn-install`

### Step 1: Set up prerequisites (one-time, per satellite region)

Before any node can join, the cluster-admin must:

1. Create a `CrossRegionNodeRole` with a `HYBRID_LINUX` access entry on the cluster
2. Establish network connectivity (TGW/peering + cluster SG allows TCP 443 from satellite VPC)
3. Register the satellite region: `xrnctl add-region --cluster-name <name> --cluster-region <region> --vpc-id <vpc> --satellite-region <region>`
4. (Only if using custom networking) Add `--with-eniconfigs` to step 3 and enable custom networking on the aws-node DaemonSet

See [docs/runbook-phase1.md](docs/runbook-phase1.md) for the full walkthrough.

### Step 2: Build user-data for AL2023

The user-data tells nodeadm how to bootstrap, then invokes `xrn-install` to apply the cross-region patches. Save as `userdata.txt`:

```
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="BOUNDARY"

--BOUNDARY
Content-Type: application/node.eks.aws

---
apiVersion: node.eks.aws/v1alpha1
kind: NodeConfig
spec:
  cluster:
    name: <cluster-name>
    region: <cluster-region>
    apiServerEndpoint: <endpoint>
    certificateAuthority: <base64-ca>
    cidr: <service-cidr>
  kubelet:
    flags:
      - --node-labels=eks.amazonaws.com/compute-type=cross-region

--BOUNDARY
Content-Type: text/x-shellscript

#!/bin/bash
# Download and run xrn-install to patch kubelet for cross-region operation
curl -sL https://<your-distribution-url>/xrn-install -o /tmp/xrn-install
chmod +x /tmp/xrn-install
/tmp/xrn-install init --cluster-name <cluster-name> --cluster-region <cluster-region>

--BOUNDARY--
```

Get the cluster values with:

```bash
aws eks describe-cluster --region <cluster-region> --name <cluster-name> \
  --query 'cluster.{endpoint:endpoint,ca:certificateAuthority.data,cidr:kubernetesNetworkConfig.serviceIpv4Cidr}'
```

> **How the two parts work together:** Part 1 (`application/node.eks.aws`) runs nodeadm, which bootstraps kubelet with the cluster endpoint/CA. Part 2 (`text/x-shellscript`) runs after nodeadm completes — `xrn-install init` detects that nodeadm already ran, skips the bootstrap step, and applies only the cross-region patches (cloud-provider, hostname, providerID, topology labels, kubeconfig region), then restarts kubelet.

### Step 3: Launch the instance

```bash
aws ec2 run-instances --region <satellite-region> \
  --image-id <eks-optimized-ami-id> \
  --instance-type <instance-type> \
  --iam-instance-profile Name=CrossRegionNodeProfile \
  --subnet-id <node-subnet-in-satellite-vpc> \
  --security-group-ids <satellite-node-sg> \
  --user-data file://userdata.txt \
  --metadata-options "HttpTokens=required,HttpPutResponseHopLimit=2,HttpEndpoint=enabled" \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=Name,Value=cross-region-node}]'
```

The node appears in `kubectl get nodes` with its instance ID as the name (e.g., `i-0a5ecec7f33053b35`) within ~90 seconds.

### What `xrn-install init` does

When invoked after nodeadm has already bootstrapped:

1. Discovers cluster config via `eks:DescribeCluster` (confirms cluster-name/region)
2. Reads node metadata from IMDS (instance ID, AZ, region)
3. Runs pre-flight checks (endpoint reachable, IMDS hop limit, cross-region confirmed)
4. Patches kubelet:
   - `--cloud-provider=""` — prevents CCM from deleting the node
   - `--hostname-override=<instance-id>` — matches `HYBRID_LINUX` identity
   - `providerID=eks-hybrid:///<cluster-region>/<cluster-name>/<instance-id>`
   - `--node-labels` with `topology.kubernetes.io/zone` and `/region`
   - kubeconfig `--region` set to cluster region (for valid STS token)
5. Restarts kubelet


## Post-join: kubelet serving certificate

After the node joins, `kubectl logs` and `kubectl exec` will fail until the kubelet's serving certificate is approved.

### Why this happens

EKS-optimized AMIs set `serverTLSBootstrap: true` in the kubelet config. On startup, kubelet submits a `kubernetes.io/kubelet-serving` CSR to the cluster and waits for approval before serving HTTPS on port 10250 (the port the control plane uses for `logs`/`exec`).

EKS auto-approves CSRs from cluster-VPC nodes, but **the auto-approver does not approve CSRs from cross-region nodes** — they sit pending forever, kubelet keeps retrying, and the node accumulates dozens of pending CSRs over time. Without an approved cert, kubelet logs show:

```
http: TLS handshake error from <ip>: no serving certificate available for the kubelet
```

### Manual approval (one-shot)

```bash
# Approve all pending CSRs from a specific satellite node
kubectl get csr -o json | \
  jq -r '.items[] | select(.spec.username=="system:node:<instance-id>") | select(.status.conditions==null or (.status.conditions|length)==0) | .metadata.name' | \
  xargs -I {} kubectl certificate approve {}

# Or approve everything pending (use with caution — approves all unprocessed CSRs cluster-wide)
kubectl get csr -o name | xargs kubectl certificate approve
```

After approval, kubelet picks up the cert within seconds and `kubectl logs` starts working.

### Permanent fix: deploy an auto-approver

The kubelet rotates its serving cert before expiry (~80% of TTL, default ~9 months). You'll need to re-approve unless you deploy an auto-approver. The community project [`kubelet-serving-cert-approver`](https://github.com/alex1989hu/kubelet-serving-cert-approver) handles this:

```bash
# Install via Helm
helm repo add kubelet-serving-cert-approver https://alex1989hu.github.io/kubelet-serving-cert-approver/
helm install kubelet-serving-cert-approver kubelet-serving-cert-approver/kubelet-serving-cert-approver \
  --namespace kubelet-serving-cert-approver --create-namespace
```

It validates each CSR's SANs against the requesting Node's `status.addresses` and approves automatically. Works for cluster-VPC and satellite nodes uniformly.

### Why disabling serverTLSBootstrap is NOT recommended

Setting `serverTLSBootstrap: false` lets kubelet self-sign its serving cert, but the control plane would have to skip TLS verification — `kubectl logs`/`exec` traffic would be unauthenticated TLS. The auto-approver is the right answer.

### Cleanup: clear out the backlog of pending CSRs

If the node has been running for a while without approval, you may have hundreds of pending CSRs:

```bash
# Delete all pending CSRs (kubelet will re-submit a fresh one)
kubectl get csr -o json | \
  jq -r '.items[] | select(.status.conditions==null or (.status.conditions|length)==0) | .metadata.name' | \
  xargs -I {} kubectl delete csr {}
```

Then either approve manually (the next CSR kubelet submits) or let the auto-approver handle it.


## Building

```bash
# Build both tools for local use
make build

# Cross-compile for linux (amd64 + arm64)
make build-linux

# Run tests
make test
```

## Architecture

See [docs/PRD-cross-region-nodes.md](docs/PRD-cross-region-nodes.md)

## Requirements

- Go 1.25+ (auto-upgrades to 1.26 for k8s client-go compatibility)
- AWS SDK v2
- k8s.io/client-go for cluster operations
- Target for `xrn-install`: AL2023 EKS-optimized AMIs (nodeadm-based bootstrap)
- Target for `xrnctl`: any machine with kubectl access to the cluster
