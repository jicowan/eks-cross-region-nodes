# eks-cross-region-nodes

Tools for joining EC2 worker nodes in satellite VPCs/regions to an EKS cluster in a different region.

> **Cross-account support is implemented and validated** (release v0.1.5). Satellites can live in
> *other AWS accounts*, not just other regions. Cross-account nodes run a dedicated
> `aws-node-satellite-<acct>-<region>` DaemonSet (no Pod Identity → VPC CNI uses the
> satellite-account instance role via IMDS) and use `compute-type=hybrid`; same-account/cross-region
> nodes ride the stock `aws-node` with `compute-type=cross-region`.
>
> **Start here:** [docs/user-guide.md](./docs/user-guide.md) — operator how-to for `xrn-install` and
> `xrnctl` (both topologies). System-level reference: [docs/architecture.md](./docs/architecture.md).

## Components

### `xrn-install`

On-node installer that bootstraps a satellite EC2 instance into a cross-region (or cross-account) EKS cluster.

**Subcommands:** `init` (post-boot: discover + preflight + nodeadm + patch + restart — the
same-account/cross-region path), `patch` (patch only, no nodeadm/restart — run from a kubelet
`ExecStartPre` for the **cross-account** pre-kubelet flow), `preflight`, `discover`. See the
[user guide](./docs/user-guide.md#xrn-install) for the cross-account boothook setup.

**What `init`/`patch` do:**
1. Discover cluster config via `eks:DescribeCluster` — assuming the cluster-account role first when `--cluster-account-role-arn` is set (cross-account; the instance role can't see a cluster in another account)
2. Discover node metadata from IMDS
3. (`init`) run pre-flight checks; (`init`) invoke `nodeadm init`
4. Patch kubelet for cross-region operation:
   - `--cloud-provider=""` (prevents CCM from deleting the node)
   - `--hostname-override=<instance-id>` (matches HYBRID_LINUX identity)
   - `providerID=eks-hybrid:///<cluster-region>/<cluster-name>/<instance-id>`
   - Topology labels via `--node-labels`
   - Kubeconfig: cluster region for STS (same-account), **or** an AssumeRole credential helper (cross-account, via `--cluster-account-role-arn`)
5. (`init`) restart kubelet

**Usage:**
```bash
# same-account / cross-region (post-boot)
xrn-install init --cluster-name my-cluster --cluster-region us-east-2

# cross-account (from a kubelet ExecStartPre drop-in; see the user guide)
xrn-install patch --cluster-name my-cluster --cluster-region us-east-2 \
  --cluster-account-role-arn arn:aws:iam::<cluster-acct>:role/XrnSatelliteNodeRole
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
- (Only if using custom networking) ENIConfig CRs created for this node's AZ via `xrnctl add-satellite --with-eniconfigs`

### `xrnctl`

Cluster-admin tool for managing satellites. Runs from an operator workstation with kubectl access to the cluster. Full reference: **[docs/user-guide.md](./docs/user-guide.md)**.

**What it does:**
- `setup-iam` — creates the node IAM role + instance profile and the `HYBRID_LINUX` access entry. For cross-account, creates **both** the satellite-account node role and the cluster-account role it assumes, wiring the trust both ways (`--profile` selects the account per run).
- `add-satellite` — maintains the `aws-node-vpc-cidrs` ConfigMap (SNAT CIDRs + registry), updates the cluster's `RemoteNetworkConfig`, and for **cross-account** renders + applies the dedicated `aws-node-satellite-<acct>-<region>` DaemonSet. Validates CIDR non-overlap.
- `remove-satellite` — deregisters a satellite; refuses if nodes are still registered.
- `list-satellites` — lists registered satellites (region, account, CIDRs, ENIConfigs).
- `verify` — detects drift between the ConfigMap, ENIConfigs, and node labels.
- Optionally creates/deletes `ENIConfig` CRs (only when custom networking is required).

**Usage:**
```bash
# Register a same-account / cross-region satellite (CIDRs auto-discovered)
xrnctl add-satellite \
  --cluster-name main --cluster-region us-east-2 \
  --vpc-id vpc-09c3d15c27ab543c5 --satellite-region eu-west-1

# Register a CROSS-ACCOUNT satellite (--account-id + --vpc-cidr required;
# xrnctl can't DescribeVpcs in another account). Renders + applies the dedicated DaemonSet.
xrnctl add-satellite \
  --cluster-name main --cluster-region us-east-2 \
  --vpc-id vpc-bbbb --satellite-region us-west-1 \
  --account-id 310444902345 --vpc-cidr 10.2.0.0/16

# List all registered satellites
xrnctl list-satellites --cluster-name main --cluster-region us-east-2

# Verify consistency
xrnctl verify --cluster-name main --cluster-region us-east-2

# Remove a satellite (fails if nodes still present)
xrnctl remove-satellite --cluster-name main --cluster-region us-east-2 --vpc-id vpc-09c3d15c27ab543c5
```

**What `add-satellite` does by default:**
- Updates `kube-system/aws-node-vpc-cidrs` ConfigMap (`exclude-snat-cidrs` + `registry.json`).
- Updates the cluster's `RemoteNetworkConfig.remoteNodeNetworks` with the satellite CIDR.
- For cross-account (`--account-id` differs from the cluster account): renders and applies the
  dedicated `aws-node-satellite-<acct>-<region>` DaemonSet (use `--dry-run` to print instead).

**With `--with-eniconfigs --security-group-ids sg-x[,sg-y]`:** additionally creates one `ENIConfig`
per AZ. Only needed when pods must use a different subnet/SG than the node; otherwise the VPC CNI
uses the node's own subnet (auto-discovered via IMDS).

**Prerequisites:**
- kubectl access to the cluster (kubeconfig configured)
- AWS credentials for the cluster account (`eks:DescribeCluster`, `eks:UpdateClusterConfig`,
  and — same-account only — `ec2:DescribeVpcs` in the satellite region). `setup-iam` additionally
  needs IAM write permissions; cross-account runs use `--profile` to target each account.

**What `verify` checks:**
- Every CIDR in `registry.json` is present in `exclude-snat-cidrs`.
- Satellites registered `--with-eniconfigs` have their `ENIConfig` CRs present.
- Every satellite node (`compute-type` in `cross-region`/`hybrid`) carries a
  `topology.kubernetes.io/zone` label. (ENIConfigs are optional, so their absence is not drift.)

## Bootstrapping a satellite node with `xrn-install`

> This walkthrough is the **same-account / cross-region** flow (post-boot `xrn-install init`).
> For **cross-account** satellites, the node must be patched *before* kubelet's first start via a
> cloud-boothook + `xrn-install patch` drop-in — see the
> [user guide](./docs/user-guide.md#cross-account-pre-kubelet-patch) and
> `deploy/asg/userdata-cross-account.template.txt`.

### Step 1: Set up prerequisites (one-time, per satellite region)

Before any node can join, the cluster-admin must:

1. Create the node IAM role + `HYBRID_LINUX` access entry: `xrnctl setup-iam --cluster-name <name> --cluster-region <region> --node-role-name CrossRegionNodeRole` (same-account). For cross-account, see the two-step flow in the [user guide](./docs/user-guide.md#setup-iam--create-iam-prerequisites).
2. Establish network connectivity (TGW/peering + cluster SG allows TCP 443 from satellite VPC)
3. Register the satellite: `xrnctl add-satellite --cluster-name <name> --cluster-region <region> --vpc-id <vpc> --satellite-region <region>` (add `--account-id` + `--vpc-cidr` for cross-account)
4. (Only if using custom networking) Add `--with-eniconfigs` to step 3 and enable custom networking on the aws-node DaemonSet

See the [user guide](./docs/user-guide.md) for the full walkthrough (both topologies).

### Step 2: Build user-data for AL2023

The user-data tells nodeadm how to bootstrap and installs a kubelet `ExecStartPre` drop-in that runs `xrn-install patch` to apply the cross-region patches before kubelet starts on every boot. Save as `userdata.txt`:

```
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="BOUNDARY"

--BOUNDARY
Content-Type: text/cloud-boothook

#!/bin/bash
# Install xrn-install and a kubelet ExecStartPre drop-in that re-applies the cross-region
# patches on every kubelet start (survives reboots — see note below).
curl -sL https://<your-distribution-url>/xrn-install -o /usr/local/bin/xrn-install
chmod +x /usr/local/bin/xrn-install
mkdir -p /etc/systemd/system/kubelet.service.d
cat > /etc/systemd/system/kubelet.service.d/99-xrn-patch.conf <<DROP
[Service]
ExecStartPre=/usr/local/bin/xrn-install patch --cluster-name <cluster-name> --cluster-region <cluster-region>
DROP
systemctl daemon-reload

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

--BOUNDARY--
```

Get the cluster values with:

```bash
aws eks describe-cluster --region <cluster-region> --name <cluster-name> \
  --query 'cluster.{endpoint:endpoint,ca:certificateAuthority.data,cidr:kubernetesNetworkConfig.serviceIpv4Cidr}'
```

> **How the parts work together:** The `application/node.eks.aws` part is consumed by nodeadm, which bootstraps kubelet with the cluster endpoint/CA. The `text/cloud-boothook` part installs a kubelet `ExecStartPre` drop-in; because `kubelet.service` is ordered `After=nodeadm-config.service`, the patch runs after the config files exist but before kubelet's main process starts. `xrn-install patch` applies the cross-region patches (cloud-provider, hostname, providerID, topology labels, kubeconfig get-token `--region`) without running nodeadm or restarting kubelet.
>
> **Why a boothook + `ExecStartPre`, not a one-shot `init` script?** `nodeadm-config.service` is `WantedBy=multi-user.target`, so it re-runs on **every** boot and regenerates `/etc/eks/kubelet/environment` and `/var/lib/kubelet/kubeconfig` from the NodeConfig above. A `text/x-shellscript` part runs only on first boot, so after any reboot (maintenance, stop/start, crash) nodeadm's regenerated kubeconfig would carry the node's local region again and kubelet could no longer authenticate to the home-region cluster. `ExecStartPre` runs on every kubelet start, so the patch is re-applied after each regeneration. (`xrn-install init` — discovery + nodeadm + patch + restart, run once post-boot — still exists for interactive use, but the ASG/launch-template path uses the boothook flow.)

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

### Step 4: Move from one-off instances to an ASG with autoscaling (recommended)

Step 3 launches a single satellite instance by hand. For production-shaped operation, you'll want an Auto Scaling group fronted by Cluster Autoscaler so satellite capacity scales with workload demand. Two ready-to-run scripts cover this:

| Goal | Path | Docs |
|---|---|---|
| Create a satellite ASG + launch template (uses the same user-data pattern as Step 2, but applied via launch template) | `deploy/asg/` | [deploy/asg/README.md](deploy/asg/README.md) |
| Install Cluster Autoscaler scoped to **only** the satellite ASG (cross-region, EKS Pod Identity auth) | `deploy/cluster-autoscaler/` | [deploy/cluster-autoscaler/README.md](deploy/cluster-autoscaler/README.md) |

**Quick recipe:**

```bash
# 1. Edit the config block at the top of deploy/asg/create-asg.sh
#    (cluster name, satellite region, VPC ID, subnet IDs, SG, instance profile, instance type)
./deploy/asg/create-asg.sh

# 2. Create the IAM role + EKS Pod Identity association for Cluster Autoscaler
./deploy/cluster-autoscaler/setup-iam.sh

# 3. Deploy Cluster Autoscaler (scoped to the ASG via --nodes=0:3:cross-region-<cluster>-asg)
kubectl apply -f deploy/cluster-autoscaler/cluster-autoscaler.yaml

# 4. (Optional) Verify scale-up works
kubectl apply -f deploy/cluster-autoscaler/test-scale.yaml
kubectl get nodes -l eks.amazonaws.com/compute-type=cross-region -w
```

After Step 4:
- New satellite nodes launch automatically via the ASG when pods are Pending
- Each new instance bootstraps via the same `xrn-install` flow from Step 2 (no manual user-data per instance)
- CA scales the ASG up to its `max` (set during ASG creation) when there's demand, and back down after `--scale-down-unneeded-time` (default 10m) of idleness
- CA runs only in the cluster region and only manages the satellite ASG (no risk of it touching other ASGs)

For multi-region satellite topologies (ASGs in 3+ regions), see the "Adding more satellite ASGs" section of [deploy/cluster-autoscaler/README.md](deploy/cluster-autoscaler/README.md).

(For what `xrn-install init`/`patch` do step-by-step, see the [`xrn-install`](#xrn-install) component section above.)

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
# Standalone (single replica)
kubectl apply -f https://raw.githubusercontent.com/alex1989hu/kubelet-serving-cert-approver/main/deploy/standalone-install.yaml

# Or HA (3 replicas with leader election)
kubectl apply -f https://raw.githubusercontent.com/alex1989hu/kubelet-serving-cert-approver/main/deploy/ha-install.yaml
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

See [docs/architecture.md](docs/architecture.md) — system-level reference (topologies, the CCM
reap, credential chains, bootstrap sequencing, data plane).

## Requirements

- Go 1.25+ (auto-upgrades to 1.26 for k8s client-go compatibility)
- AWS SDK v2
- k8s.io/client-go for cluster operations
- Target for `xrn-install`: AL2023 EKS-optimized AMIs (nodeadm-based bootstrap)
- Target for `xrnctl`: any machine with kubectl access to the cluster
