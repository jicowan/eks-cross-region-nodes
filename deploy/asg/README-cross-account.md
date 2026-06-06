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
| `${XRN_INSTALL_URL}` | URL to download the `xrn-install` binary |

## Prerequisites (operator-provisioned, out of band)

- `XrnNodeRole` + instance profile in the **satellite** account (use `xrnctl setup-iam --profile <satellite> --node-role-only`)
- `XrnSatelliteNodeRole` in the **cluster** account, trusting the satellite node role, with a
  `HYBRID_LINUX` access entry (use `xrnctl setup-iam --profile <cluster> --access-entry-only --node-role-arn <satellite-role>`)
- The dedicated `aws-node-satellite-<acct>-<region>` DaemonSet applied to the cluster
  (`xrnctl add-satellite --account-id <satellite-acct> …`)
- TGW peering + routes, cluster SG inbound from the satellite CIDR (DNS/pods/kubelet), and the
  satellite VPC CIDR in the cluster's `RemoteNetworkConfig` (the last one is handled by `add-satellite`)
