# Cross-Account Architecture: Findings from First Empirical Test

**Date:** 2026-06-05
**Test setup:**
- Cluster `main` in account 820537372947, us-east-2
- Satellite VPC `vpc-0ad34a89ceb5a2ab0` (10.2.0.0/16) in account 310444902345, us-west-1
- TGW peering between cluster VPC and satellite VPC, routes verified

This document captures empirical findings from the first attempt at joining a cross-account satellite node. Some of these contradict the PRD `PRD-cross-account-nodes.md`. The PRD must be revised before Phase 5a coding starts.

## What worked

- ✅ The launch template + ASG infrastructure builds cleanly in the satellite account
- ✅ The EC2 instance role's `sts:AssumeRole` permission, scoped to the cluster-account satellite role
- ✅ The `XrnSatelliteNodeRole` in the cluster account holds the `HYBRID_LINUX` access entry (same-account, satisfies EKS's principal validation)
- ✅ The kubelet successfully authenticated to EKS using AssumeRole'd credentials and registered the Node object briefly
- ✅ Cluster-account network reachability via TGW (the bootstrap script downloaded xrn-install over the public internet, kubelet reached the cluster API endpoint)

## What broke

### Finding 1: `HYBRID_LINUX` access entries reject cross-account principals

**Confirmed by error message:**
```
AccessEntry principalArn must be from the same account as the cluster when using
types [EC2_LINUX, EC2_WINDOWS, FARGATE_LINUX, HYBRID_LINUX, EC2]
```

The original PRD §4 design — a `HYBRID_LINUX` access entry directly on the satellite account's `XrnNodeRole` — is impossible. EKS rejects cross-account principals on all EC2-flavored access entry types.

**The workaround:** create a "satellite role" in the *cluster* account, give it cross-account trust (allowing the EC2 instance role in the satellite account to assume it), and put the `HYBRID_LINUX` access entry on the cluster-account role. The EC2 instance assumes into the cluster-account role at boot; kubelet uses those credentials.

This works but is structurally heavier than the PRD described. The auth chain is now: EC2 instance profile → STS AssumeRole → cluster-account role → kubelet identity to EKS.

### Finding 2: `role_session_name` must equal the instance ID

When the AWS SDK does AssumeRole via `credential_source = Ec2InstanceMetadata`, it auto-generates a session name like `botocore-session-1780675640`. The EKS-issued kubelet identity becomes `system:node:botocore-session-1780675640`.

But kubelet's `--hostname-override` is the instance ID (`i-0a26086747676a29d`) — required to match the `HYBRID_LINUX` access entry's `system:node:{{SessionName}}` template, which we previously confirmed expects the session name to equal the hostname.

Result: the Node authorizer rejects every kubelet API call with:
```
node 'botocore-session-1780675640' cannot read 'i-0a26086747676a29d',
only its own Node object
```

**Fix:** add `role_session_name = $INSTANCE_ID` to `/root/.aws/config`. Setting this manually fixed the issue and the node successfully registered.

This is a NEW patch beyond the existing 5 cross-region kubelet patches. It must be in the user-data, not in `xrn-install` (since `xrn-install` doesn't manage the AWS profile).

### Finding 3: `AWS_PROFILE` set at the host level breaks aws-node (VPC CNI)

The bootstrap script wrote `/root/.aws/config` and ran `xrn-install` with `AWS_PROFILE=default`. This:

1. **Broke `xrn-install`'s own preflight checks.** Calls to `ec2:DescribeInstances` (for IMDSv2 hop-limit verification) and `ec2:DescribeVpcs` (for CIDR overlap check) routed through the AssumeRole'd profile, which targets us-east-2. The instance and VPC are in us-west-1 in account 310444902345. Errors:
   ```
   InvalidInstanceID.NotFound: The instance ID 'i-0a26086747676a29d' does not exist
   InvalidVpcID.NotFound: The vpc ID 'vpc-0ad34a89ceb5a2ab0' does not exist
   ```
   `xrn-install init` aborted before applying any patches. The 5 patches had to be applied manually via SSM.

2. **Broke aws-node IPAMD.** Even though `xrn-install` exits, `/root/.aws/config` persists on the host. The aws-node DaemonSet pod's `aws-k8s-agent` process picks up the profile and tries to call `ec2:DescribeNetworkInterfaces` for the satellite ENIs through the cluster-account role in us-east-2:
   ```
   ec2:DescribeNetworkInterfaces, get identity: get credentials: failed to refresh
   cached credentials, [...]: (InvalidTokenException): The token included in the
   request is invalid
   ```
   IPAMD fails initialization, the pod restarts in a loop, no CNI plugin is registered, no pods can be scheduled on the node.

**Architectural conflict:** kubelet wants cluster-account credentials (us-east-2). aws-node wants satellite-account credentials (us-west-1). A host-level AWS profile cannot serve both.

### Finding 4: `RemoteNetworkConfig` updates delete existing satellite Node objects

Already documented in PRD-cross-region-nodes.md §14. Confirmed again here: adding `10.2.0.0/16` to `RemoteNetworkConfig` deleted the existing eu-west-1 satellite Node object (`i-08a6756f2a7b4163e`), even though the eu-west-1 CIDR (`10.1.0.0/16`) was retained. EKS reconciles all hybrid nodes against any change to RemoteNetworkConfig.

This is a real operational gotcha for any environment that wants to add satellite regions/accounts without disruption. Without auto-recovery (kubelet doesn't auto-re-register), every existing satellite node needs a kubelet restart after each RemoteNetworkConfig change.

### Finding 5: The cloud-controller-manager deletes nodes whose providerID it can't validate

Recorded event:
```
DeletingNode  node/i-0a26086747676a29d  Deleting node i-0a26086747676a29d
              because it does not exist in the cloud provider
```

Even with `--cloud-provider=""` (empty) and `providerID=eks-hybrid:///...` — the patches that prevent this in the same-account case — the EKS CCM still deleted the cross-account satellite node ~30 seconds after registration.

**Hypothesis (not yet verified):** the EKS-managed CCM validates the providerID against the cluster account's EC2 inventory. For same-account satellite nodes, the instance ID in the providerID corresponds to a real instance the CCM can find. For cross-account satellite nodes, the instance ID is in a different account, and the CCM's lookup fails.

If true, this is a fundamental limit on the cross-account approach: even with all the right config, the CCM treats cross-account instances as "ghost nodes" and reaps them.

This needs further investigation. The eu-west-1 same-account satellite ran for 24+ hours without CCM deletion (it was only deleted by the RemoteNetworkConfig update, not by the periodic CCM scan), so the CCM tolerates same-account hybrid nodes indefinitely. Cross-account behavior may differ.

## Implications for the PRD

The PRD as written assumes:
1. Cross-account works with one `--cluster-account-role-arn` flag on `xrn-install` ❌
2. Pod Identity cross-account is "just two env vars" for CA ⚠️  (likely still true, but the broader cross-account story is harder than this implied)
3. The architecture is a small extension of Phases 1–4 ❌

The actual architecture requires:
1. A cluster-account role to hold the access entry (and trust the satellite role)
2. AssumeRole credential chain for kubelet (with explicit `role_session_name`)
3. **Separation** of credentials between kubelet and aws-node (same host, two different identities)
4. Possibly something to prevent CCM from deleting cross-account nodes (this might require an EKS feature request, not just configuration)

## Decisions to make

1. **Is the architectural conflict (Finding 3) solvable without code changes to the VPC CNI or kubelet?**
   - Option A: Configure kubelet's kubeconfig to invoke a credential-process script that does AssumeRole inline. No host-level AWS profile. aws-node uses default IMDS creds (satellite account).
   - Option B: Set `AWS_PROFILE=` only as a kubeconfig `exec.env` value. Same effect as A, less moving code.
   - Option C: Patch the VPC CNI to read its own credential profile (e.g., from a separate file). Requires VPC CNI changes.

2. **Is Finding 5 (CCM deleting cross-account nodes) a hard wall or a soft one?**
   - We need to verify whether the CCM is actually doing cross-account EC2 validation, or if the deletion was triggered by something else (e.g., the kubelet failure to update the node lease in time after the auth issue). If we can keep a cross-account node healthy on first try, we'll know.

3. **If both Finding 3 and Finding 5 are hard walls, is cross-account out of scope for the project?**
   - Possible alternative: document that cross-account is unsupported and the only supported topology is "satellite VPCs in the cluster's account, possibly across regions."

## Next steps (set by user)

- Try **Option A** for Finding 3: credential-process script in kubelet kubeconfig, no host-level AWS profile.
- If Option A doesn't work, try **Option B**.
- If neither works, retire the cross-account PRD as "design proven non-viable in this iteration."

## Update 2026-06-05 (after Option A test)

### Finding 6: VPC CNI credentials come from a cluster-account Pod Identity association

Option A (host-level `/root/.aws/config` removed; kubelet uses a credential-process script for AssumeRole) **did fix kubelet auth** (kubelet successfully registered the node) but **did not fix aws-node IPAMD**.

Root cause discovered:

```
aws eks describe-pod-identity-association --association-id a-so7kasueteylb9hwk
{
  "namespace": "kube-system",
  "serviceAccount": "aws-node",
  "roleArn": "arn:aws:iam::820537372947:role/AmazonEKSPodIdentityAmazonVPCCNIRole",
  "ownerArn": "arn:aws:eks:us-east-2:820537372947:addon/main/vpc-cni/..."
}
```

The cluster's `vpc-cni` EKS addon created a Pod Identity association binding the `kube-system/aws-node` ServiceAccount to a cluster-account IAM role. The eks-pod-identity-agent runs as a DaemonSet on every node — including the satellite — and intercepts AWS credential requests from any pod using that ServiceAccount.

When `aws-k8s-agent` (the IPAMD daemon inside aws-node) calls `ec2:DescribeNetworkInterfaces`:
1. The AWS SDK queries the Pod Identity Agent (via the standard `AWS_CONTAINER_CREDENTIALS_FULL_URI` env var injected into the pod)
2. The agent returns credentials for `AmazonEKSPodIdentityAmazonVPCCNIRole` in account 820537372947
3. IPAMD uses those credentials to call EC2 in us-west-1
4. EC2 rejects with `InvalidTokenException` — the role doesn't exist in 310444902345, the token is signed in a different account context

This cannot be solved by removing files on the host or scoping kubelet's AWS_PROFILE. The credentials are delivered out-of-band by an agent process designed specifically to override SDK credential resolution.

### Finding 7: The Pod Identity association cannot be selectively scoped per node pool

Possible fixes considered:

1. **Delete the aws-node Pod Identity association.** Would break aws-node on cluster-VPC nodes (where it actually works correctly). Non-starter.
2. **Use Pod Identity's `--target-role-arn` for cross-account on aws-node.** Pod Identity associations are per-ServiceAccount, not per-node. Both cluster-VPC nodes and satellite nodes share the `aws-node` ServiceAccount. Setting a target-role-arn would route ALL aws-node pods through it, including the cluster-VPC ones — same problem in reverse.
3. **Run a separate aws-node DaemonSet for satellite nodes with a different ServiceAccount.** Possible, but requires forking the upstream VPC CNI's manifests, scoping each by nodeSelector, and creating a separate Pod Identity association for the satellite-only SA. The satellite SA's Pod Identity would need to deliver credentials that work in the satellite account — which means either:
   - A satellite-account IAM role with VPC CNI permissions
   - Cross-account Pod Identity (`--target-role-arn`) targeting that role
4. **Patch the VPC CNI to allow disabling Pod Identity / falling back to IMDS.** Code change to upstream. Out of scope.

### Reframing: this is solvable if we accept the same trade-off hybrid nodes make

EKS Hybrid Nodes face an analogous situation — they exist in a "different account" from the cluster's perspective (an on-prem identity has no AWS account at all) — and EKS supports them. The key insight: **hybrid nodes don't run the VPC CNI**. They use Cilium or Calico, which the EKS docs explicitly require. That sidesteps Wall 2 entirely.

This means the architecture for cross-account satellite nodes should mirror what hybrid nodes do, not what same-account same-region satellites do. Specifically:

#### Reconsidering Wall 1 (CCM deletion)

Hybrid nodes' providerID is `eks-hybrid:///<region>/<cluster-name>/<node-name>` and they are NOT reaped by the CCM, even though they're not real EC2 instances in the cluster account. So the CCM is not doing what we assumed.

Possible explanations worth testing:
- **The CCM checks if the node's IP is in `RemoteNetworkConfig.RemoteNodeNetworks` and skips EC2 validation if so.** Our cross-account satellite IP (`10.2.141.200`) IS in `RemoteNodeNetworks` (we added `10.2.0.0/16`). If this is the discriminator, the deletion would be a timing issue — the CCM saw the node *before* the patches were applied (when `--cloud-provider=external` was still set, kubelet briefly had nodeadm's default config), decided to delete it, and the patches arrived too late.
- **A label or annotation specific to hybrid nodes** (`eks.amazonaws.com/hybrid-node`?) marks the node as exempt. Worth grepping the EKS docs.
- **The order of operations matters.** If kubelet starts with the right config from the very first second (no nodeadm-default → patch sequence), the CCM may never see a "wrong" state.

Testable: if `xrn-install` runs **before** kubelet starts (i.e., we modify the AL2023 boot order so nodeadm-config and our patches both complete before kubelet.service starts), the node may persist.

#### Reconsidering Wall 2 (VPC CNI auth)

The right answer is **don't run VPC CNI on cross-account satellite nodes.** Use the same approach hybrid nodes use:

- Run Cilium or Calico (configurable per-node via DaemonSet nodeSelector / taints) on satellite nodes
- VPC CNI continues running on cluster-VPC nodes
- Trade-off: pods on cross-account satellite nodes get IPs from the chosen CNI's IPAM (overlay or static), NOT from the VPC. Cross-region pod-to-pod traffic flows over an overlay or via L3 routes the customer configures.

This is a real architectural change with consequences:
- Lose the "real VPC IP per pod" property on satellite nodes only
- Gain functioning networking that doesn't depend on VPC CNI auth
- Need to document the topology clearly: cluster-VPC nodes use VPC CNI; cross-account satellite nodes use Cilium/Calico
- Network policies, security groups, observability tooling that assumes VPC IPs all need adjustment for satellite pods

#### Updated conclusion

Cross-account is **harder than same-account but not architecturally impossible**, IF we accept:
1. Cross-account satellite nodes don't run the VPC CNI — use Cilium or Calico instead (matches the hybrid-nodes pattern)
2. The bootstrap sequencing problem (kubelet must never start with the default nodeadm config) needs solving — likely by patching `/etc/kubernetes/kubelet/config.json` and `/etc/eks/kubelet/environment` *before* the kubelet systemd service runs, not after
3. Documentation needs to clearly delineate which CNI runs where and what the tradeoffs are

This pushes Phase 5 from "small extension of Phases 1–4" to a **larger redesign**:
- New CNI selection logic in the satellite bootstrap (skip VPC CNI, install Cilium/Calico)
- New ConfigMap entries to declare which satellite VPCs use which CNI
- Operator documentation for choosing CNI per topology
- Per-node CNI is supported but adds operational complexity

### Decision pending

Reframing aside, this is a substantive scope expansion. Options:

1. **Reduce ambition:** keep cross-account out of scope for this project; document the path above as "what would be needed to add it later"
2. **Phase 5 redesign:** treat cross-account as a separate workstream that introduces multi-CNI topology, mirroring what EKS Hybrid Nodes does. This is no longer "add a flag to xrn-install" — it's a new capability with its own design considerations
3. **Spike on Wall 1 first:** before redesigning, test the "patches before kubelet starts" theory. If timing is the issue, the same-CNI path might still work and Wall 2 is the only one we have to address. If timing isn't the issue, we know Wall 1 is genuine and we have to solve both walls.

Recommend (3): one more empirical test (patch the AMI / boot order so kubelet never starts with the wrong config) to determine whether Wall 1 is timing-dependent or fundamental. Result of that test informs whether Phase 5 is small or large.

## Update 2026-06-05 (Wall 1 timing test result)

### Wall 1 is timing-dependent — partially solvable

Spike completed using a `cloud-boothook` user-data section that:
1. Drops in `/etc/systemd/system/kubelet.service.d/99-xrn-wait.conf` — `ExecStartPre` blocks kubelet startup until `/run/xrn-ready` exists
2. Installs `xrn-patch.service` (oneshot, ordered After=`nodeadm-config.service` Before=`kubelet.service`) that applies the 5 patches + credential helper, then creates the sentinel

Initial result: the cross-account node **persisted past the previous 30s deletion window** with no `DeletingNode` event. The kubelet starts the first time with `--cloud-provider=""`, `--hostname-override=<instance-id>`, and `providerID=eks-hybrid:///...` — confirming the CCM does respect `eks-hybrid:///` providerIDs for cross-account nodes when the patches are applied before kubelet's first start.

**However**: the node was eventually deleted anyway, ~10 minutes later. The deletion path was different from the original CCM-EC2-lookup deletion. With aws-node failing IPAMD initialization (Wall 2), the node remained `NotReady`, no CNI plugin registered, and a different EKS-side controller decided to remove it. The exact reaper isn't confirmed — could be a node-not-ready timeout, could be the cluster's autoscaler treating it as unschedulable, could be EKS-managed addon controllers reacting to the broken state.

The takeaway: **fixing the timing is necessary but not sufficient.** Wall 1 (CCM EC2 lookup) is solved by the timing fix. But there's a Wall 1.5 — secondary node-deletion paths triggered by Wall 2's downstream effects — that means the node still doesn't persist long-term. Those paths likely close on their own once aws-node is healthy, but we won't know until we fix Wall 2.

**Operational implication:** the runbook's "apply patches via xrn-install after nodeadm completes" approach is incompatible with cross-account. Cross-account requires the patches to happen between nodeadm and kubelet, not after kubelet has already registered. This is a structural change to the bootstrap sequence — but it can be done entirely with cloud-boothook + a systemd drop-in, no AMI fork required.

### Wall 2 (VPC CNI Pod Identity) remains unsolved

The aws-node DaemonSet still fails IPAMD initialization on the cross-account node. The Pod Identity Agent intercepts SDK credential requests and returns cluster-account credentials; aws-node tries to use them in us-west-1 to manage satellite-account ENIs; EC2 rejects with `InvalidTokenException`.

Possible solutions, ranked by feasibility:

1. **Run a separate aws-node DaemonSet for satellite nodes with a dedicated ServiceAccount.** That ServiceAccount has no Pod Identity association, so aws-node falls through to the EC2 instance role (satellite account). Requires:
   - A second `aws-node-satellite` DaemonSet manifest with `nodeSelector: eks.amazonaws.com/compute-type=cross-region`
   - The original `aws-node` DaemonSet gets a `nodeAntiAffinity` excluding satellite nodes (so they don't both run on satellite nodes)
   - Both DaemonSets share image and config; only the SA differs
2. **Use a separate Pod Identity association with `--target-role-arn` for the satellite SA**, pointing at a role in the satellite account. This delivers satellite-account creds via Pod Identity instead of IMDS, which is cleaner if you want least-privilege scoping.
3. **Run a different CNI on satellite nodes** (Cilium / Calico). Matches the EKS Hybrid Nodes pattern. Pods lose VPC-native IPs on satellite nodes only.

Option 1 is the lightest. Worth trying before option 3.

### Updated conclusion

Cross-account is **viable** with two caveats:
1. Bootstrap requires `cloud-boothook` + drop-in (one-time complexity at install time)
2. aws-node needs a satellite-only DaemonSet variant (one-time complexity at cluster-config time)

Neither requires forking nodeadm or building a custom AMI. Both are operational additions to the runbook. The PRD should be updated to reflect this rather than rejected.

## Update 2026-06-05 (Wall 1.5 root cause + Wall 2 confirmed fix — full path validated)

### Wall 1.5 was the cloud_node_lifecycle_controller, not providerID validation

With control-plane logs enabled, the actual reap path was traced in CCM logs for `i-0e0c0d43f67fc5555` (cross-region label):

```
18:04:42.551  describe_instance_batch.go:89] Batched describe instances [i-0e0c0d43f67fc5555]
18:04:42.632  ERROR DescribeInstances ... InvalidInstanceID.NotFound
18:04:42.704  node_lifecycle_controller.go:187] deleting node since it is no longer
              present in cloud provider: i-0e0c0d43f67fc5555
18:04:42.705  reason=DeletingNode message="Deleting node ... because it does not
              exist in the cloud provider"
```

Two CCM controllers continuously try to look up the node in EC2:
1. **`tagging_controller`** — calls `CreateTags` to apply `aws:eks:cluster-name`. Fails on cross-account but only logs errors, never escalates to deletion.
2. **`node_lifecycle_controller`** — runs `DescribeInstances` after a 2-minute grace period. On `InvalidInstanceID.NotFound`, deletes the Node object.

Both controllers use the EKS-managed CCM's IAM role (in the cluster account). Neither can see instances in the satellite account. The `eks-hybrid:///` providerID is irrelevant to this path — that providerID gates a *different* check (the cloud_node_controller's initial validation). Lifecycle is a separate continuous loop.

**Why same-account/cross-region nodes worked:** in same-account, `DescribeInstances` succeeds (the cluster-account CCM owns the instance, just in a different region). The lifecycle controller is satisfied. In cross-account, `DescribeInstances` *fundamentally cannot* succeed without changing CCM credentials.

### Update 2026-06-05 (later): the `hybrid`-label hypothesis is more nuanced than first thought

A new same-account/cross-region satellite (`i-0eed68968ff19161a`, eu-west-1, `compute-type=cross-region`) was reaped at 19:03:38 with the same `node_lifecycle_controller.go:187 deleting node since it is no longer present in cloud provider` line we saw in the cross-account run. Initial reading: same-account/cross-region is broken too, and `hybrid` is the necessary fix.

**That reading was wrong, on two counts:**

1. **Phases 1–4 same-account/cross-region satellites have been observed surviving** with the `cross-region` label across multiple deployments. The 19:03 reap is not the typical outcome.
2. **The `hybrid`-labeled node still receives `DescribeInstances NotFound` calls from CCM** (CWL inspection of `i-044d29bbb51ca319f` shows them at 18:24, 18:29, 18:34, ... continuing for the lifetime of the node) and is *not* reaped. So `hybrid` is not making CCM skip the call — something else prevents the deletion despite the call returning `NotFound`. The "lifecycle controller skips entirely on hybrid" claim earlier in this doc was unsupported by the actual logs.

The 19:03 reap of `i-0eed68968ff19161a` is therefore explained by something other than "CCM regional DescribeInstances." Likely confounders not yet ruled out:
- The eu-west-1 node never reached Ready (the `aws-node` DS was patched earlier in this session to exclude `cross-region` from its NodeAffinity NotIn list, so no CNI scheduled on it). A NotReady satellite Node may have a different reap path than a Ready one.
- Other lifecycle paths (lease expiration, condition-based delete) may be at play.

**What is actually known empirically (2026-06-05 evening):**
- A `hybrid`-labeled cross-account node receives ongoing `NotFound` from CCM and is not reaped — i.e., something about its state prevents the lifecycle controller from acting on the `NotFound`. Mechanism uncharacterized.
- A `cross-region`-labeled same-account node *can* be reaped after `NotFound`. Whether this happens reliably or only when the node is also NotReady is not yet established.

**Updated implication:** the label change in Phase 5e is for consistency across topologies (one DS, one bootstrap path), not a fix for a confirmed defect in same-account/cross-region. Existing same-account deployments do not need to migrate urgently.

### Path A: `compute-type=hybrid` label correlates with surviving the lifecycle reap (mechanism uncharacterized)

CCM's `node_lifecycle_controller.go:172` honors a skip path for nodes labeled `eks.amazonaws.com/compute-type=hybrid`. Mirrors what real EKS Hybrid Nodes do — the controller never calls `DescribeInstances` for them.

**Test: relabel from `cross-region` to `hybrid` and observe.**

For instance `i-044d29bbb51ca319f` (hybrid label):
```
I0605 18:21:25 Successfully registered node
I0605 18:21:27..18:23:02 (every 5s) skipping node ... within grace period (2m0s)
I0605 18:23:02+ — controller goes silent, NEVER runs DescribeInstances, NEVER reaps
```

Node persists Ready indefinitely. Same boothook + same xrn-patch.sh as before, only the kubelet `--node-labels` changed. Wall 1.5: **solved**.

### Wall 2 confirmed fix (separate `aws-node-satellite` DaemonSet)

Confirmed empirically on `i-044d29bbb51ca319f`:
- `aws-node-satellite-m2qtj` pod scheduled (DS partitioned by `nodeAffinity`: original `aws-node` adds `hybrid` to its `NotIn` list; satellite DS uses `In: [hybrid]`)
- Pod runs under `aws-node-satellite` ServiceAccount with no Pod Identity association
- AWS SDK falls through to IMDS, picks up the EC2 instance role (`XrnNodeRole` in account 310444902345)
- IPAMD calls `ec2:DescribeNetworkInterfaces` in us-west-1 successfully:
  ```
  18:05:24.181  DescribeAllENIs success: ENIs: 1, tagged: 1
  18:05:24.181  initialized datastore for network cards index 0
  18:05:24.181  ENI eni-0675207b01ae48bec set up
  18:05:24.182  Found sg-0888e111d29d09354, added to ipamd cache
  ```
- Pod reaches `2/2 Running` (both `aws-node` and `aws-eks-nodeagent` healthy, gRPC `:50051` responding to readiness probes)

**Why one DS isn't enough:** Pod Identity associations bind to ServiceAccounts, not nodes. The original `aws-node` SA has a cluster-managed Pod Identity association (created by the EKS `vpc-cni` addon) pointing at `AmazonEKSPodIdentityAmazonVPCCNIRole` in the cluster account. On satellite nodes, the Pod Identity Agent still injects those cluster-account creds into any pod under that SA — wrong account for managing satellite-account ENIs.

The two node fleets fundamentally need different credential sources, so two ServiceAccounts (and therefore two DaemonSets) are required. The original `aws-node` DS is left untouched (zero patching of EKS-managed manifests); the satellite DS runs alongside it.

### Final architecture summary (working)

| Wall | Cause | Fix |
|------|-------|-----|
| 1: CCM cloud_node validation | Default `--cloud-provider=external` + `aws:///` providerID makes CCM expect a same-account EC2 instance | `--cloud-provider=""` + `eks-hybrid:///<region>/<cluster>/<id>` providerID, **applied before kubelet first-start** (cloud-boothook + systemd drop-in `ExecStartPre` blocking on `/run/xrn-ready` sentinel) |
| 1.5: CCM node_lifecycle DescribeInstances reap | Lifecycle controller calls `DescribeInstances` against cluster-account EC2; instance is in satellite account; gets `NotFound`; deletes Node | Set `eks.amazonaws.com/compute-type=hybrid` label on the node — lifecycle controller skips these (mirrors real EKS Hybrid Nodes behavior) |
| 2: VPC CNI Pod Identity intercept | EKS addon-created Pod Identity association on `aws-node` SA returns cluster-account creds; IPAMD uses them in satellite region; EC2 rejects | Run separate `aws-node-satellite` DS under a SA with no Pod Identity association; SDK falls through to IMDS; gets satellite-account instance role |
| Tagging spam | `tagging_controller` continuously fails `CreateTags` for cross-account instance | **Non-fatal** — controller logs errors but doesn't delete the Node. Ignored. |

**Scope of cross-account changes vs same-account/cross-region:**
- `cloud-boothook` + systemd drop-in (replaces post-boot xrn-install patch path)
- AssumeRole credential helper for kubelet (instance role → cluster-account satellite role → kubelet identity)
- Label change: `compute-type=cross-region` → `compute-type=hybrid`
- `aws-node-satellite` DaemonSet + ServiceAccount + ClusterRoleBinding (parallel to original `aws-node`)

**Known operational quirk:** `xrn-patch.service` doesn't auto-start at boot (its `After=nodeadm-config.service` ordering doesn't fire when `nodeadm-config` has already completed by the time the unit is enabled). Currently kicked manually via SSM. To productionize, fold the patch script body directly into the cloud-boothook (boothook → apply patches inline → `touch /run/xrn-ready` → exit), eliminating the separate systemd unit.
