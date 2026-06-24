# Architecture

How EC2 worker nodes in other VPCs, regions, and AWS accounts join a single EKS cluster and run
the VPC CNI. This is the system-level reference; for operator how-to see the
[user guide](./user-guide.md).

## 1. Problem and shape

One EKS cluster. Worker nodes (typically GPU) in *additional* VPCs that may be in other regions
and/or other AWS accounts, joined over the customer's TGW/peering. Workloads stay region-local;
only kubelet ↔ API server (and control-plane → kubelet) crosses the boundary. No second cluster,
no federation.

```
                       ┌──────────────────────────────────────────┐
                       │  EKS cluster — account A, region A        │
                       │   control plane (CCM, kube-apiserver)     │
                       │   cluster VPC 10.0.0.0/16                  │
                       │   aws-node (stock DS) + Pod Identity       │
                       │   aws-node-vpc-cidrs ConfigMap (registry)  │
                       │   RemoteNetworkConfig.remoteNodeNetworks   │
                       └───────────────┬────────────────────────────┘
                                       │  customer TGW / peering
            ┌──────────────────────────┼───────────────────────────────┐
            │                          │                                │
   ┌────────┴───────────┐    ┌─────────┴────────────┐      ┌────────────┴───────────────┐
   │ same-account       │    │ same-account         │      │ CROSS-ACCOUNT               │
   │ region B           │    │ region C             │      │ account D, region E         │
   │ 10.1.0.0/16        │    │ ...                  │      │ 10.2.0.0/16                 │
   │ label: cross-region│    │                      │      │ label: hybrid               │
   │ rides stock aws-node│   │                      │      │ aws-node-satellite-<D>-<E>  │
   └────────────────────┘    └──────────────────────┘      └─────────────────────────────┘
```

## 2. Components

| Component | Where it runs | Role |
|---|---|---|
| **`xrn-install`** | on each satellite node (user-data) | Discovers the cluster, patches kubelet for cross-region/cross-account, installs the AssumeRole credential helper (cross-account). Binary. |
| **`xrnctl`** | operator workstation | Cluster-side state: IAM (`setup-iam`), satellite registry + `RemoteNetworkConfig` + per-account DaemonSet (`add-satellite`), drift checks (`verify`). Binary. |
| **stock `aws-node`** | cluster-VPC + same-account satellite nodes | EKS-managed VPC CNI DaemonSet. Untouched by this project. |
| **`aws-node-satellite-<acct>-<region>`** | cross-account satellite nodes | Dedicated VPC CNI DaemonSet, rendered by `xrnctl`. SA has no Pod Identity association. |
| **`aws-node-vpc-cidrs` ConfigMap** | cluster (kube-system) | Canonical satellite registry + the SNAT-exclusion CIDR list consumed by the CNI. |
| **ConfigMap SNAT watcher** | inside the VPC CNI (separate repo: amazon-vpc-cni-k8s) | Reads the ConfigMap and updates SNAT exclusions at runtime. Gated by `AWS_VPC_K8S_CNI_ENABLE_DYNAMIC_SNAT_CFG`. |

## 3. The two topologies

The single most important distinction in the system. It is driven by **where the node's AWS
account is relative to the cluster**, and it determines the CNI DaemonSet, the node label, and the
credential path.

| | Same-account / cross-region | Cross-account |
|---|---|---|
| Satellite account | **same** as cluster | **different** |
| Node label | `eks.amazonaws.com/compute-type=cross-region` | `eks.amazonaws.com/compute-type=hybrid` |
| VPC CNI DaemonSet | stock `aws-node` | dedicated `aws-node-satellite-<acct>-<region>` |
| CNI's AWS creds | Pod Identity (cluster-account role) | IMDS (satellite-account instance role) |
| kubelet → API auth | instance role directly | instance role → AssumeRole cluster-account role |
| Bootstrap timing | pre-kubelet (`xrn-install patch` via ExecStartPre) | pre-kubelet (`xrn-install patch` via ExecStartPre) |

### 3.1 Why the labels differ (DaemonSet routing)

The two `compute-type` values are **load-bearing**, exploiting EKS's stock behavior — no edit to
the EKS-managed `aws-node` DS is needed in either case:

- Stock `aws-node` nodeAffinity **already excludes `compute-type=hybrid`** (EKS ships it that way;
  hybrid nodes are expected to run Cilium/Calico, not the VPC CNI).
- Stock `aws-node` does **not** exclude `cross-region` (a value EKS has never heard of).

So a same-account satellite labeled `cross-region` is picked up by stock `aws-node` automatically
(and Pod Identity works because it's the same account). A cross-account satellite labeled `hybrid`
is auto-excluded from stock `aws-node` and instead matches the dedicated DS's nodeAffinity. If we
unified on `hybrid`, same-account satellites would also be excluded from stock `aws-node` and would
need their own DS — extra machinery for no benefit.

### 3.2 Why cross-account needs its own DaemonSet (Wall 2)

EKS Pod Identity binds credentials to a **ServiceAccount**, not a node. The stock `aws-node` SA has
a Pod Identity association that returns **cluster-account** credentials. On a cross-account node
those credentials can't manage ENIs in the satellite account — IPAMD's EC2 calls fail with
`InvalidTokenException`. The fix is a parallel DaemonSet whose ServiceAccount has **no** Pod
Identity association; the AWS SDK then falls through to IMDS and uses the satellite-account instance
role (`XrnNodeRole`), which has the CNI policy. One DS per `(account, region)` pair, because
`AWS_REGION` and the SNAT-exclusion CIDRs are baked into the DS spec.

## 4. Node registration and the CCM reap (Walls 1 / 1.5)

The EKS cloud-controller-manager (CCM) is the central obstacle. It runs two relevant loops, both
querying EC2 **in the cluster's region** with the **cluster account's** role:

- **cloud_node_controller** — validates a node's providerID at registration.
- **node_lifecycle_controller** — after a ~2-minute grace, calls `DescribeInstances`; if the
  instance "doesn't exist," it deletes the Node object (`DeletingNode`).

A satellite instance is invisible to both (wrong region, and for cross-account, wrong account). The
defenses, applied by `xrn-install`:

1. `--cloud-provider=""` (not `external`) — disables CCM integration on the kubelet side.
2. `providerID` — see the format decision below.
3. `--hostname-override=<instance-id>` — so the node name matches the `system:node:<id>` identity.
4. Topology labels (`topology.kubernetes.io/region`/`zone`).

**The decisive factor for cross-account (validated 2026-06-06):** the providerID must be in place
**before kubelet's first registration**. CCM continues to log `DescribeInstances`/`NotFound` for the
node, but with the right providerID set in time it does not act on it. This is why cross-account uses
the pre-kubelet ordering (§6.2): cross-account gets no grace window, so a post-boot patch races the
reaper.

### providerID format: `aws:///` (same-account) vs `eks-hybrid:///` (cross-account)

The providerID format is chosen by `xrn-install --provider-id-format` (`aws` or `eks-hybrid`):

| | Same-account / cross-region | Cross-account |
|---|---|---|
| providerID | `aws:///<az>/<instance-id>` | `eks-hybrid:///<region>/<cluster>/<id>` |
| CCM reaps it? | No — **with `--cloud-provider=""`** the lifecycle controller does not delete it (validated 2026-06-24) | No — the hybrid providerID is not reaped |
| Cluster Autoscaler can manage it? | **Yes** — CA's AWS provider parses `aws:///<az>/<id>` and matches it to the ASG instance | **No** — CA can't parse `eks-hybrid:///`, classifies the node `longUnregistered`, and deletes it on a ~15min loop |

**Why the split.** Originally both topologies used `eks-hybrid:///`, because that's what the CCM
lifecycle controller demonstrably does not reap. But that format is unparseable by the
Cluster Autoscaler AWS provider (`AwsRefFromProviderId` requires `aws:///<zone>/<name>`), so CA
churned the satellite ASGs. The 2026-06-24 test showed that for **same-account** nodes the
`eks-hybrid:///` providerID was never actually necessary — what prevents reaping is
`--cloud-provider=""`, not the providerID prefix. So same-account uses `aws:///` (CCM-safe **and**
CA-manageable). Cross-account still uses `eks-hybrid:///`: the CCM is more aggressive across accounts
and `aws:///` there is unverified, so cross-account ASGs are **not** autoscaled by CA (run them
static or with an ASG-native policy). See [`deploy/cluster-autoscaler/README.md`](../deploy/cluster-autoscaler/README.md).

## 5. Credential chains

### 5.1 Same-account / cross-region

`EC2 instance → instance role (CrossRegionNodeRole, cluster account)`. The role holds a
`HYBRID_LINUX` access entry, so kubelet authenticates directly via `aws eks get-token`. The VPC CNI
uses Pod Identity (stock `aws-node`). One account throughout.

### 5.2 Cross-account

Two parallel chains:

**kubelet identity (4 hops):**
```
EC2 → XrnNodeRole (satellite acct)            [EC2 trust, instance profile]
    → XrnSatelliteNodeRole (cluster acct)     [AssumeRole; trust + node-role grant]
    → eks:DescribeCluster / eks get-token     [permission on the cluster role]
    → system:node:<instance-id>               [HYBRID_LINUX access entry on the cluster role]
```
The session name on the AssumeRole is the instance ID, so the EKS-issued identity is
`system:node:<instance-id>` (matching `--hostname-override`). The access entry's principal must be a
**cluster-account** role — EKS rejects cross-account principals on `HYBRID_LINUX` entries — which is
why the satellite instance assumes `XrnSatelliteNodeRole` rather than holding the entry directly.

**VPC CNI / IPAMD:** uses the satellite-account instance role via IMDS (no Pod Identity), calling
EC2 in the satellite region directly. No cross-account hop.

**Application pods needing AWS access in the satellite account** use cross-account Pod Identity
(`--target-role-arn`), not IMDS. A plain Pod Identity association on a cross-account node returns
*cluster-account* credentials, which is usually the wrong account — see
[pod-identity-cross-account.md](./pod-identity-cross-account.md) for the full explanation and setup.

`xrnctl setup-iam` provisions this whole chain across two profile-scoped runs (see user guide):
satellite-account run creates `XrnNodeRole` + instance profile + the AssumeRole grant; cluster
account run creates `XrnSatelliteNodeRole` with the cross-account trust + `eks:DescribeCluster` + the
access entry.

## 6. Bootstrap sequencing

### 6.1 Same-account (pre-kubelet, ExecStartPre)

A `cloud-boothook` lays down a kubelet `ExecStartPre` drop-in that runs `xrn-install patch
--cluster-name … --cluster-region …` (no `--cluster-account-role-arn` — the instance role reaches
the cluster's EKS API directly). The patch sets cloud-provider/hostname/providerID/labels and
rewrites the kubeconfig get-token `--region` to the home region; no AssumeRole helper is installed.

**Why ExecStartPre and not a one-shot `init` script.** `nodeadm-config.service` is
`WantedBy=multi-user.target`, so it re-runs on **every** boot and regenerates
`/etc/eks/kubelet/environment` and `/var/lib/kubelet/kubeconfig` from the NodeConfig. A run-once
user-data script (`text/x-shellscript`, cloud-init per-instance) executes only on first boot, so
after any reboot — maintenance, EC2 stop/start, crash — nodeadm's regenerated kubeconfig carries the
node's *local* region again. Since the get-token token is a region-pinned SigV4 presigned request,
the home-region authenticator then rejects it and kubelet can't re-authenticate. `ExecStartPre` runs
on every kubelet start (ordered `After=nodeadm-config.service`), so the patch is re-applied after
each regeneration and the node survives reboots. (`xrn-install init` — discovery + nodeadm + patch +
restart — remains for interactive/manual bootstrap, but the ASG path uses the boothook flow.)

### 6.2 Cross-account (pre-kubelet, ExecStartPre-as-gate)

A `cloud-boothook` lays down a kubelet `ExecStartPre` drop-in:
```
ExecStartPre=/usr/local/bin/xrn-install patch --cluster-name … --cluster-region … \
             --cluster-account-role-arn arn:aws:iam::<cluster-acct>:role/XrnSatelliteNodeRole
```
`kubelet.service` is ordered `After=nodeadm-config.service`, so the config files already exist when
`ExecStartPre` runs, and kubelet's main process will not start until `patch` succeeds. This makes
kubelet's own unit ordering the synchronization gate — no separate systemd unit, no sentinel file.
`xrn-install patch` does discovery (assuming the cluster-account role, since the instance role can't
see a cluster in another account), applies the same kubelet patches as `init`, and installs the
AssumeRole credential helper — then exits, releasing kubelet to start with correct config on its
first registration.

## 7. Data plane

Pods get VPC-native IPs from the satellite VPC via the VPC CNI (custom networking / ENIConfig is
opt-in; by default the CNI uses the node's own subnet). For traffic to stay source-IP-correct across
VPCs, both the cluster VPC CIDR and every satellite VPC CIDR are added to the CNI's SNAT exclusion
list (`exclude-snat-cidrs` in the ConfigMap). Reachability requires:

- The satellite VPC CIDR in the cluster's `RemoteNetworkConfig.remoteNodeNetworks` (set by
  `add-satellite`) so the control plane can reach kubelet on :10250.
- Security-group rules both ways (cluster SG inbound from satellite CIDR on 443/53/pods/10250;
  satellite SG inbound from cluster CIDR on pods/10250).
- A kubelet-serving CSR approver for `kubectl logs/exec` (EKS does not auto-approve satellite-node
  serving CSRs).

Validated end-to-end (2026-06-06): a cross-account satellite pod resolves CoreDNS, reaches the
kube-apiserver Service, reaches a cluster-VPC pod with its source IP intact, and reaches the
internet. Satellite-to-satellite traffic is out of scope (hub-and-spoke only).

**Persistent storage** on satellite nodes is more constrained than networking — EBS is
AZ/region/account-locked and the EBS CSI controller hits the same credential/region walls as the
CNI (and additionally requires a forked, renamed driver to run per-account). Node-local storage
(EC2 Instance Store CSI) is the clean fit because it makes no AWS API calls. See
[storage-options.md](./storage-options.md).

**Exposing satellite pods through a load balancer** uses the AWS Load Balancer Controller's
`TargetGroupBinding` with `ip` target type: a pre-provisioned NLB/ALB registers satellite pod IPs
(routable over the TGW), with built-in fields for cross-VPC (`vpcID`) and cross-account
(`iamRoleArnToAssume`) target groups. The auto-provision-from-annotations path is single-VPC only.
See [load-balancing.md](./load-balancing.md).

## 8. What this project does NOT change

- The VPC CNI's IPAM model (custom networking already does the right thing per-region).
- The EKS-managed `aws-node` DaemonSet (left untouched; routing is via labels + stock exclusions).
- Networking underlay (TGW/peering/routes), security groups, and IAM trust are customer/operator
  preconditions — `xrnctl setup-iam` creates the IAM *roles and entries*, but not the cross-account
  network path.
