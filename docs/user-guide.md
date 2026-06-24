# User Guide: `xrn-install` and `xrnctl`

This guide covers the two tools for running EKS worker nodes in VPCs/regions — and optionally
**AWS accounts** — other than the cluster's own.

- **`xrnctl`** — cluster-admin tool. Run from your workstation (cluster-admin kubeconfig + AWS
  credentials). Manages cluster-side state: the satellite registry, ENIConfigs,
  `RemoteNetworkConfig`, the per-account VPC-CNI DaemonSet, and IAM (roles, instance profiles,
  access entries).
- **`xrn-install`** — on-node installer. Runs in the worker node's user-data. Discovers the
  cluster, patches kubelet for cross-region/cross-account operation, and (for cross-account)
  installs an AssumeRole credential helper.

Two topologies are supported:

| Topology | Satellite location | CNI DaemonSet | Node label |
|---|---|---|---|
| **Same-account / cross-region** | a different region, **same** AWS account as the cluster | rides the cluster's stock `aws-node` | `eks.amazonaws.com/compute-type=cross-region` |
| **Cross-account** | a different AWS account (any region) | a dedicated `aws-node-satellite-<acct>-<region>` DS | `eks.amazonaws.com/compute-type=hybrid` |

> **Why the difference?** The stock `aws-node` ServiceAccount has an EKS Pod Identity association
> that returns *cluster-account* credentials. That's fine same-account, but on a cross-account
> node those credentials can't manage ENIs in the satellite account. So cross-account nodes run a
> dedicated DaemonSet whose ServiceAccount has **no** Pod Identity association — the VPC CNI then
> falls through to IMDS and uses the satellite-account instance role.

---

## Prerequisites (customer-provided)

These are preconditions the tools do **not** create:

- **Networking:** TGW (or VPC peering) connecting the cluster VPC to each satellite VPC, with
  routes in both directions. Non-overlapping CIDRs across the cluster and all satellites.
- **Security groups:**
  - Cluster SG: inbound from each satellite VPC CIDR on TCP 443 (API), TCP+UDP 53 (CoreDNS),
    TCP 1024–65535 (pod-to-pod), TCP 10250 (kubelet).
  - Satellite SG: inbound from the cluster VPC CIDR on TCP 1024–65535 and TCP 10250.
- **Cluster auth mode:** `API` or `API_AND_CONFIG_MAP` (access entries are required; the
  `aws-auth` ConfigMap does not support the `HYBRID_LINUX` type).
- **A kubelet-serving CSR auto-approver** if you want `kubectl logs/exec` to satellite pods
  (EKS does not auto-approve these for satellite nodes). See the README "Post-join: kubelet serving
  certificate" section.

---

## `xrnctl`

Global flags on every subcommand:

- `--cluster-name NAME` `--cluster-region REGION` — required.
- `--profile PROFILE` — use a named AWS profile instead of the default credential chain.
  Essential for cross-account: run `setup-iam` once per account with the matching profile.

### `setup-iam` — create IAM prerequisites

Creates the node IAM role + instance profile and/or the `HYBRID_LINUX` access entry. Idempotent.

**Same-account (one run, cluster-account credentials):**
```bash
xrnctl setup-iam \
  --cluster-name main --cluster-region us-east-2 \
  --node-role-name CrossRegionNodeRole
```
Creates `CrossRegionNodeRole` + matching instance profile (worker/CNI/ECR/SSM managed policies,
EC2 trust) **and** the `HYBRID_LINUX` access entry for it — all in the cluster account.

**Cross-account (two runs, two profiles).** The cross-account auth chain is:
`EC2 → XrnNodeRole (satellite acct) → XrnSatelliteNodeRole (cluster acct) → kubelet identity`.
`setup-iam` creates **both** roles and wires the trust in both directions. Pick predictable names 
up front; the cluster role's ARN is referenced by both runs.

```bash
SAT_ACCT=310444902345
CLUSTER_ACCT=820537372947
NODE_ROLE_ARN=arn:aws:iam::$SAT_ACCT:role/XrnNodeRole
SAT_ROLE_ARN=arn:aws:iam::$CLUSTER_ACCT:role/XrnSatelliteNodeRole

# 1) SATELLITE account — node role + instance profile, plus an inline policy granting it
#    sts:AssumeRole on the (not-yet-created) cluster-account role.
xrnctl setup-iam --profile satellite \
  --cluster-name main --cluster-region us-east-2 \
  --node-role-name XrnNodeRole --node-role-only \
  --satellite-role-arn "$SAT_ROLE_ARN"

# 2) CLUSTER account — create XrnSatelliteNodeRole with a trust policy allowing the
#    satellite node role to assume it, grant it eks:DescribeCluster, and attach the
#    HYBRID_LINUX access entry. One command does all three.
xrnctl setup-iam --profile cluster \
  --cluster-name main --cluster-region us-east-2 \
  --create-satellite-role \
  --trusted-node-role-arn "$NODE_ROLE_ARN" \
  [--satellite-role-name XrnSatelliteNodeRole] \
  [--external-id <id>]
```

After step 2, `xrn-install` on the satellite node can: assume `XrnNodeRole` (EC2), assume
`XrnSatelliteNodeRole` (granted in step 1, trusted in step 2), call `eks:DescribeCluster`
(granted in step 2), and register via the `HYBRID_LINUX` access entry (created in step 2).

> The access entry's principal is always a **cluster-account** role. EKS rejects cross-account
> principals on `HYBRID_LINUX` entries, so a cross-account satellite's kubelet assumes a
> cluster-account role (`XrnSatelliteNodeRole`) and that role holds the access entry.

**Flags:**
- `--node-role-name` — node role to create/reuse (satellite account).
- `--node-role-only` / `--access-entry-only` — do just one half of the *same-account* split (mutually exclusive).
- `--node-role-arn` — for `--access-entry-only`, the principal the access entry is for.
- `--satellite-role-arn` — step 1: grant the node role `sts:AssumeRole` on this (cluster-account) ARN.
- `--create-satellite-role` — step 2: create the cluster-account role (its own mode).
- `--trusted-node-role-arn` — step 2 (required): the satellite-account node role allowed to assume it.
- `--satellite-role-name` — step 2: role name to create (default `XrnSatelliteNodeRole`).
- `--external-id` — step 2: optional `sts:ExternalId` required by the trust policy (confused-deputy protection).

> If you set `--external-id` in step 2, also pass the same value to `xrn-install` via
> `--cluster-account-external-id` (and to the boothook's `patch` invocation) so the AssumeRole matches.

**External-id (optional, recommended for cross-org):** when the satellite and cluster accounts are
owned by different teams, set `--external-id` so the cluster role can only be assumed with the
agreed secret. Thread the same value through `xrn-install --cluster-account-external-id`.

### `add-satellite` — register a satellite VPC

Updates the `aws-node-vpc-cidrs` ConfigMap (SNAT exclusions + registry), updates the cluster's
`RemoteNetworkConfig`, and — for cross-account — renders and applies the dedicated DaemonSet.
Idempotent (re-running re-applies the DS and re-checks `RemoteNetworkConfig`).

**Same-account / cross-region:**
```bash
xrnctl add-satellite \
  --cluster-name main --cluster-region us-east-2 \
  --vpc-id vpc-aaaa --satellite-region eu-west-1
```
(The satellite VPC's CIDRs are discovered automatically via `DescribeVpcs`.)

**Cross-account** — `--account-id` and `--vpc-cidr` are required (xrnctl runs with
cluster-account credentials and cannot look up a VPC in another account):
```bash
xrnctl add-satellite \
  --cluster-name main --cluster-region us-east-2 \
  --vpc-id vpc-bbbb --satellite-region us-west-1 \
  --account-id 310444902345 --vpc-cidr 10.2.0.0/16
```
This applies `aws-node-satellite-310444902345-us-west-1` (SA with no Pod Identity association +
ClusterRoleBinding to the stock `aws-node` ClusterRole + DaemonSet scoped to nodes labeled
`compute-type=hybrid` + `satellite-account=310444902345` + `region=us-west-1`).

Useful flags:
- `--dry-run` — render and print the DaemonSet manifest; **no** cluster changes at all.
- `--with-eniconfigs --security-group-ids sg-x[,sg-y] [--subnet-ids ...]` — opt into custom
  networking (only when pods must use a different subnet/SG than the node). Off by default;
  pods otherwise get IPs from the node's own subnet.

### `list-satellites` / `remove-satellite` / `verify`

```bash
xrnctl list-satellites --cluster-name main --cluster-region us-east-2
# VPC ID                 REGION       ACCOUNT        CIDRs          ENICONFIGS
# vpc-aaaa               eu-west-1    (cluster)      10.1.0.0/16    -
# vpc-bbbb               us-west-1    310444902345   10.2.0.0/16    -

xrnctl remove-satellite --cluster-name main --cluster-region us-east-2 --vpc-id vpc-bbbb
# refuses if nodes from that VPC are still registered — drain/terminate them first

xrnctl verify --cluster-name main --cluster-region us-east-2
# checks ConfigMap / ENIConfig / node-label consistency; reports drift
```

---

## `xrn-install`

Runs on the node (in user-data). Subcommands:

| Subcommand | What it does |
|---|---|
| `init` | discovery + preflight + `nodeadm` + patch + restart kubelet. The **post-boot** path (same-account / cross-region). |
| `patch` | apply the kubelet patches **only** — no `nodeadm`, no restart. The **pre-kubelet** path; run from a `kubelet.service` `ExecStartPre`. Required for cross-account. |
| `preflight` | run the pre-flight checks, exit non-zero on failure. |
| `discover` | print discovered cluster config as JSON; applies nothing. |

Flags: `--cluster-name`, `--cluster-region` (required); `--cluster-account-role-arn` and
`--cluster-account-external-id` (cross-account); `--provider-id-format {eks-hybrid|aws}`
(default `eks-hybrid`).

### Same-account / cross-region (pre-kubelet `patch`, `--provider-id-format aws`)

User-data (AL2023 MIME multipart) sets the `cross-region` label via NodeConfig, then a
`cloud-boothook` installs a kubelet `ExecStartPre` drop-in that runs `patch` before kubelet's
first (and every) start:
```bash
curl -sLfo /usr/local/bin/xrn-install "$XRN_URL" && chmod +x /usr/local/bin/xrn-install
mkdir -p /etc/systemd/system/kubelet.service.d
cat > /etc/systemd/system/kubelet.service.d/99-xrn-patch.conf <<DROP
[Service]
ExecStartPre=/usr/local/bin/xrn-install patch --cluster-name main --cluster-region us-east-2 --provider-id-format aws
DROP
systemctl daemon-reload
```
The `ExecStartPre` flow (not a run-once `init`) makes the patch reboot-durable — `nodeadm-config`
regenerates the kubelet config on every boot, and `ExecStartPre` re-applies the patch each time.

`--provider-id-format aws` (the default) writes the standard `aws:///<az>/<id>` providerID. This is
required for **Cluster Autoscaler** to manage these nodes (CA only understands `aws:///`; with
`eks-hybrid:///` it deletes them as `longUnregistered`). With `--cloud-provider=""` the EKS CCM does
not reap the node despite the `aws:///` providerID — true for cross-account too (see below).

Template: `deploy/asg/userdata.template.txt`. (`init` — discovery + nodeadm + patch + restart, run
once post-boot — still exists for interactive/manual bootstrap.)

### Cross-account (pre-kubelet `patch`)

A cross-account node must have its providerID and config in place **before** kubelet first
registers (it gets no CCM grace). A `cloud-boothook` lays down a kubelet `ExecStartPre` drop-in:
```bash
# in the cloud-boothook
curl -sLfo /usr/local/bin/xrn-install "$XRN_URL" && chmod +x /usr/local/bin/xrn-install
mkdir -p /etc/systemd/system/kubelet.service.d
cat > /etc/systemd/system/kubelet.service.d/99-xrn-patch.conf <<EOF
[Service]
ExecStartPre=/usr/local/bin/xrn-install patch --cluster-name main --cluster-region us-east-2 --cluster-account-role-arn arn:aws:iam::<cluster-acct>:role/XrnSatelliteNodeRole --provider-id-format aws
EOF
systemctl daemon-reload
```
`kubelet.service` is ordered `After=nodeadm-config.service`, so the config files exist when
`ExecStartPre` runs, and kubelet's main process won't start until `patch` succeeds. `patch`:

1. Assumes the cluster-account role for `eks:DescribeCluster` (the instance role can't see a
   cluster in another account).
2. Sets `--cloud-provider=""`, `--hostname-override=<instance-id>`, topology labels, and
   `providerID=aws:///<az>/<instance-id>` (validated 2026-06-24: CCM does not reap this cross-account
   with `--cloud-provider=""`; also lets a per-account CA manage the ASG. Pass
   `--provider-id-format eks-hybrid` for the legacy format).
3. Installs the AssumeRole credential helper (`/etc/kubernetes/xrn/get-cluster-token.sh`) and
   points the kubelet kubeconfig at it — kubelet authenticates by assuming the cluster-account
   role with session name = instance ID (so its identity is `system:node:<instance-id>`).

The NodeConfig part sets `--node-labels=eks.amazonaws.com/compute-type=hybrid,xrn.amazonaws.com/satellite-account=<acct>`.

Template: `deploy/asg/userdata-cross-account.template.txt` (see `deploy/asg/README-cross-account.md`).

---

## End-to-end: onboard a cross-account satellite

1. **Network + SG + auth** prerequisites in place (above).
2. **IAM** (both roles + trust both ways, two runs):
   ```bash
   # satellite account: node role + instance profile + AssumeRole grant on the cluster role
   xrnctl setup-iam --profile satellite --cluster-name main --cluster-region us-east-2 \
     --node-role-name XrnNodeRole --node-role-only \
     --satellite-role-arn arn:aws:iam::<cluster-acct>:role/XrnSatelliteNodeRole
   # cluster account: create XrnSatelliteNodeRole (trust + eks:DescribeCluster) + access entry
   xrnctl setup-iam --profile cluster --cluster-name main --cluster-region us-east-2 \
     --create-satellite-role \
     --trusted-node-role-arn arn:aws:iam::<satellite-acct>:role/XrnNodeRole
   ```
3. **Register the satellite + apply the CNI DaemonSet:**
   ```bash
   xrnctl add-satellite --cluster-name main --cluster-region us-east-2 \
     --vpc-id vpc-bbbb --satellite-region us-west-1 \
     --account-id 310444902345 --vpc-cidr 10.2.0.0/16
   ```
4. **Launch the ASG** in the satellite account with the cross-account user-data template
   (point `${XRN_INSTALL_URL}` at a released `xrn-install` binary).
5. **Verify:**
   ```bash
   kubectl get nodes -l eks.amazonaws.com/compute-type=hybrid
   xrnctl verify --cluster-name main --cluster-region us-east-2
   ```
   A healthy node is `Ready`, runs an `aws-node-satellite-<acct>-<region>` pod (2/2), and is not
   reaped after the ~2-minute CCM grace.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Node registers then disappears after ~2 min (`DeletingNode`) | kubelet started with `--cloud-provider=external` (not `""`) before the patch ran | Cross-account must use the `patch`/`ExecStartPre` flow, not `init`, so `--cloud-provider=""` is set before first kubelet start. Check `/var/log/xrn-boothook.log` and `journalctl -u kubelet`. (Reaping is prevented by `--cloud-provider=""`, not the providerID format.) |
| `xrn-install patch`: `ResourceNotFoundException: No cluster found` | Missing/incorrect `--cluster-account-role-arn` (discovery ran with the satellite-account instance role) | Pass the cluster-account role ARN; ensure the instance role can assume it. |
| satellite `aws-node` pod `InvalidTokenException` on EC2 calls | pod is using cluster-account creds (Pod Identity) | Confirm it's the dedicated `aws-node-satellite-*` DS (its SA has no Pod Identity association), not stock `aws-node`. |
| `add-satellite`: `InvalidVpcID.NotFound` | cross-account VPC lookup with cluster-account creds | Pass `--vpc-cidr` explicitly (required when `--account-id` is set). |
| `kubectl logs/exec` to a satellite pod times out | no `RemoteNetworkConfig` entry, no serving-cert approver, or SG rule missing | `add-satellite` sets `RemoteNetworkConfig`; install a kubelet-serving CSR approver; open SG 10250 from the cluster CIDR. |
| Pods on satellite can't reach CoreDNS / cluster Services | cluster SG missing inbound from satellite CIDR | Add TCP+UDP 53 and TCP 1024–65535 from the satellite VPC CIDR to the cluster SG. |
