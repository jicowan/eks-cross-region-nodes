# Cross-account satellite ASG

This directory has two user-data templates:

| Template | Use for | Bootstrap timing |
|---|---|---|
| `userdata.template.txt` | same-account / cross-region satellites | **post-boot** — kubelet starts, then `xrn-install init` patches and restarts it |
| `userdata-cross-account.template.txt` | **cross-account** satellites | **pre-kubelet** — `xrn-install patch` runs from a kubelet `ExecStartPre` drop-in, so the first kubelet start already has the correct providerID |

## Why two templates

A same-account/cross-region node survives a brief mis-registration: CCM gives it a grace
window, and `xrn-install init` patches the providerID before the window closes. A
cross-account node does **not** get that grace — CCM reaps it the moment it sees a node
whose instance isn't in the cluster account's regional EC2 inventory. So the providerID
(`eks-hybrid:///…`) must be in place *before* the very first kubelet registration.

The cross-account template achieves that with a `cloud-boothook` that:

1. Downloads `xrn-install`.
2. Writes `/etc/systemd/system/kubelet.service.d/99-xrn-patch.conf` with:
   ```
   ExecStartPre=/usr/local/bin/xrn-install patch --cluster-name … --cluster-region … --cluster-account-role-arn …
   ```
   `kubelet.service` is ordered `After=nodeadm-config.service`, so the config files already
   exist when `ExecStartPre` runs, and kubelet's main process won't start until the patch
   succeeds. No separate systemd unit, no sentinel file — kubelet's own ordering is the gate.

`xrn-install patch` (vs `init`) only discovers + patches; it does not run nodeadm (already
done by nodeadm-config) and does not restart kubelet (it's mid-startup). It auto-detects the
account mismatch and installs the AssumeRole credential helper for kubelet auth.

## Substitution variables

`userdata-cross-account.template.txt` expects these to be substituted (e.g. by `create-asg.sh`):

| Variable | Meaning |
|---|---|
| `${CLUSTER_NAME}` | EKS cluster name |
| `${CLUSTER_REGION}` | cluster region |
| `${CLUSTER_ENDPOINT}` | API server endpoint (from `eks:DescribeCluster`) |
| `${CLUSTER_CA}` | base64 cluster CA |
| `${CLUSTER_SERVICE_CIDR}` | service CIDR |
| `${SATELLITE_ROLE_ARN}` | `arn:aws:iam::<cluster-acct>:role/XrnSatelliteNodeRole` — assumed for kubelet creds |
| `${SATELLITE_ACCOUNT_ID}` | the satellite account ID (sets the node label used by the dedicated DaemonSet's nodeAffinity) |
| `${EXTERNAL_ID}` | optional `sts:ExternalId`; leave empty unless the cluster role's trust policy requires one (must match `xrnctl setup-iam --external-id`) |
| `${XRN_INSTALL_URL}` | URL to download the `xrn-install` binary |

## Prerequisites

IAM (both roles + bidirectional trust, created by `xrnctl setup-iam` — see the
[user guide](../../docs/user-guide.md#setup-iam--create-iam-prerequisites)):

```bash
# satellite account: node role + instance profile + AssumeRole grant on the cluster role
xrnctl setup-iam --profile <satellite> --cluster-name <name> --cluster-region <region> \
  --node-role-name XrnNodeRole --node-role-only \
  --satellite-role-arn arn:aws:iam::<cluster-acct>:role/XrnSatelliteNodeRole
# cluster account: create XrnSatelliteNodeRole (trust + eks:DescribeCluster) + access entry
xrnctl setup-iam --profile <cluster> --cluster-name <name> --cluster-region <region> \
  --create-satellite-role \
  --trusted-node-role-arn arn:aws:iam::<satellite-acct>:role/XrnNodeRole
```

Cluster state:

- The dedicated `aws-node-satellite-<acct>-<region>` DaemonSet applied to the cluster, plus the
  satellite VPC CIDR in the cluster's `RemoteNetworkConfig` — both handled by
  `xrnctl add-satellite --account-id <satellite-acct> --vpc-cidr <cidr> …`.

Operator-provisioned (out of band):

- TGW peering + routes between the cluster and satellite VPCs.
- Cluster SG inbound from the satellite CIDR (TCP 443, TCP+UDP 53, TCP 1024–65535, TCP 10250);
  satellite SG inbound from the cluster CIDR (TCP 1024–65535, TCP 10250).
