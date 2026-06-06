# Storage options for satellite nodes

Persistent storage for pods on cross-region / cross-account satellite nodes is more constrained
than networking. This explains why, which storage drivers work cleanly on satellite nodes and which
don't, and what to use for which workload.

> **Status:** design analysis. The networking path (VPC CNI) is built and validated; the storage
> guidance here is reasoned from how each driver and the underlying AWS storage service work, and
> from the same cross-account "walls" we hit and solved for the VPC CNI and Cluster Autoscaler. The
> node-local drivers (§3) are architecturally sound for satellites but have not been load-tested on
> a satellite node the way the CNI was.

## TL;DR

| Storage | Works on satellite nodes? | Why |
|---|---|---|
| **EC2 Instance Store CSI** (`lis.csi.aws.com`) | ✅ cleanly, no extra work | Node-local NVMe; **no AWS API calls**, no credentials, no region. Ephemeral. |
| **local static provisioner** (sig-storage) | ✅ cleanly, no extra work | Pre-mounted local disks; no cloud API calls. Static, ephemeral. |
| **EBS CSI** (`ebs.csi.aws.com`) | ⚠️ only with significant work (per-account controller + a **forked, renamed** driver) | Controller makes region- and account-scoped EC2 API calls; volumes are AZ/region/account-locked. |
| **EFS / FSx / S3** | depends — networked, reachable over TGW | Their own region/account scoping; not block storage. Out of scope here. |

**Recommendation:** for the satellite (typically GPU) tier, use **EC2 Instance Store** for
scratch/checkpoints and keep durable data in a networked store (S3/FSx). Avoid dynamically
provisioned EBS on satellite nodes unless you genuinely need it — see §2 for why it's costly.

## 1. The constraint that bounds everything: EBS is AZ-, region-, and account-scoped

Before any driver question, EBS itself imposes hard limits:

- A volume lives in **one Availability Zone** and can attach only to an instance **in that same AZ**.
  No cross-AZ attach, let alone cross-region.
- A volume lives in **one account**.

So "EBS in a multi-region/multi-account environment" cannot mean a volume shared across regions or
accounts. It can only mean *each satellite has its own local EBS*, and a pod using an EBS PVC is
**pinned to the AZ — and therefore the account and region — where its volume lives.** It cannot
fail over to another region without abandoning the volume. That is an EBS property; no driver or
tooling changes it.

> **AZ-name trap:** AZ names are randomized per account. `us-west-1a` in the cluster account and
> `us-west-1a` in a satellite account are usually *different physical AZs*. It's consistent within a
> single account, so this only bites if you reason about zones across accounts.

## 2. Why the EBS CSI driver is hard on satellite nodes

The EBS CSI driver has two halves:

- **Node plugin** (`ebs-csi-node`, DaemonSet) — mount/format/stage on the node. Minimal AWS API
  use. Runs fine on satellite nodes.
- **Controller** (`ebs-csi-controller`, Deployment) — does **all** the EC2 API work: `CreateVolume`,
  `AttachVolume`, `DeleteVolume`, snapshots, resize. This is where the problems are.

The controller hits the same two walls we already solved for the VPC CNI and Cluster Autoscaler:

1. **Credentials.** The stock controller's ServiceAccount has an EKS Pod Identity association →
   a **cluster-account** role. It can't create/attach EBS in a *satellite* account with
   cluster-account credentials (the `InvalidToken`/AccessDenied class of failure).
2. **Region.** The controller builds one EC2 client for one region. A volume requested in the
   satellite region won't be served by a controller pointed at the cluster region. Like Cluster
   Autoscaler, that means **one controller per `(account, region)`**.

By analogy to `aws-node-satellite`, the fix would be a dedicated EBS CSI stack per satellite
account+region, pinned to that satellite's nodes via `nodeAffinity`, running on the satellite nodes
so it picks up the satellite-account instance role via IMDS and the correct region for free, with a
`WaitForFirstConsumer` StorageClass + `allowedTopologies` to keep PVCs in the satellite's zones.

### 2.1 The part that makes EBS materially harder than the VPC CNI: the provisioner name

The `aws-node-satellite` solution reused the **stock VPC CNI image** and changed only manifests
(a separate ServiceAccount + DaemonSet). EBS cannot do that, because of how CSI identity works:

- The driver name `ebs.csi.aws.com` is the driver's **identity**, consistent across four places: the
  driver's `GetPluginInfo` gRPC response, `node-driver-registrar`'s registration with kubelet, the
  `CSIDriver` object, and `StorageClass.provisioner`.
- The `csi-provisioner` sidecar learns the name **from the driver** (via `GetPluginInfo`) and only
  acts on PVCs whose StorageClass matches. You cannot rename it from a StorageClass or a sidecar flag.
- In the upstream AWS EBS CSI driver the name is a **compile-time constant** — there is no
  `--driver-name` runtime flag or Helm value to override it.
- The `csi-provisioner` sidecars use **leader election**, so multiple controller Deployments sharing
  `ebs.csi.aws.com` leave exactly **one** active cluster-wide — and it can only talk to one region.
  There is no per-region/per-topology PVC filtering for a shared provisioner name.

Therefore, routing satellite PVCs to a satellite controller requires **distinct provisioner names**
per `(account, region)` — e.g. `ebs.csi.aws.com/<satellite>` — which requires **forking the driver,
changing the constant, and rebuilding the image**, then keeping the `CSIDriver`, registrar,
StorageClass, and the external sidecars (provisioner/attacher/resizer/snapshotter) consistent and
node-scoped (stock driver excluded from satellite nodes; satellite driver excluded from cluster
nodes).

That is a fork-and-maintain burden (rebuild every upstream release, track CVEs) — a real step up
from the manifest-only CNI change, and usually a signal to choose a different storage primitive for
the satellite tier.

> Confidence note: the "no runtime override for the driver name" claim is from the driver's design
> as of this writing (knowledge cutoff Jan 2026), not a freshly verified release. If you pursue the
> EBS path, verify the exact driver version's constants/flags in source first.

## 3. What works cleanly: node-local storage

Both node-local options sidestep **every** EBS problem for one reason: **they make no AWS API
calls**, so there are no credentials and no region to get wrong. Architecturally they're the same
shape as the VPC CNI *node* plugin that already worked cross-account: node-local, IMDS/local-only,
account-agnostic. No per-account split, no fork.

### 3.1 Amazon EC2 Instance Store CSI driver (`lis.csi.aws.com`)

From the AWS docs: it *"interacts with backend NVMe devices through local Linux operations. No AWS
API calls are involved, so no IAM role is required."* It manages the lifecycle of the instance's
local NVMe disks and exposes them as PersistentVolumes.

- Installed as the EKS add-on `aws-ec2-local-instance-store-csi-driver`.
- One driver, one provisioner name, works uniformly on cluster-VPC and satellite nodes in any
  region/account — because every operation is local to the node it runs on.
- Use `volumeBindingMode: WaitForFirstConsumer`.
- **Ephemeral:** data is lost when the node terminates. Not available on some instance types, and
  not on Fargate / EKS Auto Mode. Volume expansion unsupported.

Ideal for: GPU scratch space, data-loader shuffle buffers, local checkpoints (with async upload to
S3). This is the recommended satellite-tier block storage.

### 3.2 sig-storage local static provisioner

Detects pre-mounted local disks on each node and creates PVs for them; runs as a per-node DaemonSet,
no cloud API calls, no credentials. **Static** — you pre-provision/format/mount the disks yourself;
it does not manage NVMe lifecycle. Lower-level than the AWS LIS CSI add-on; prefer LIS CSI on AWS
unless you have a specific reason to manage the disks manually. Also ephemeral.

## 4. Durable storage that must survive the node

Instance store is ephemeral and EBS is AZ/region/account-locked, so for data that must outlive a
satellite node or be reachable from multiple places, use a networked store reachable over the
TGW/peering:

- **S3** — object storage; the natural home for training data and checkpoints. Region/account-scoped
  but accessible via API from anywhere with credentials + a route.
- **FSx for Lustre** — common for GPU training datasets; can be linked to an S3 bucket.
- **EFS** — NFS; region-scoped, multi-AZ within its region.

Each has its own region/account scoping and its own CSI driver considerations (an FSx/EFS CSI
controller making AWS API calls would hit the same credential/region walls as EBS — evaluate per
driver), but none give you roaming *block* storage. If a workload needs durable storage that roams
across regions, block storage (EBS) is the wrong primitive; design around object/networked storage.

## 5. Decision guide

| Workload need on a satellite node | Use |
|---|---|
| Fast local scratch / shuffle / ephemeral checkpoints | **EC2 Instance Store CSI** (`lis.csi.aws.com`) |
| Local disk you provision and manage yourself | sig-storage local static provisioner |
| Durable data shared with the cluster / other regions | S3 (or FSx/EFS), over the TGW |
| Dynamically provisioned EBS, satellite-local, node-durable | EBS CSI — only if truly required; needs a per-`(account,region)` controller and a **forked, renamed** driver (§2.1) |
| Block storage that roams across regions | Not possible with EBS — rearchitect around object/networked storage |
