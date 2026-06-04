# Satellite ASG / Launch Template

Scripts to create an EC2 Auto Scaling Group in a satellite region for cross-region EKS nodes. Each instance launched by the ASG bootstraps via `nodeadm` then runs `xrn-install` to apply the cross-region kubelet patches.

## Files

| File | Purpose |
|---|---|
| `userdata.template.txt` | MIME multipart user-data template — Part 1 is the NodeConfig for nodeadm; Part 2 downloads `xrn-install` from your GitHub release and runs it |
| `create-asg.sh` | Creates a launch template and ASG in the satellite region |

## Prerequisites

Before running `create-asg.sh`, complete the cluster-side setup (see `../docs/runbook-phase1.md`):

1. `RemoteNetworkConfig` includes the satellite VPC CIDR
2. `HYBRID_LINUX` access entry on the cluster for `CrossRegionNodeRole`
3. Cluster security group allows TCP 443 from the satellite VPC CIDR
4. TGW peering / routes between cluster and satellite VPCs
5. SNAT exclusion CIDRs set via `xrnctl add-region`
6. **xrn-install released** to <https://github.com/jicowan/eks-cross-region-nodes/releases>
   - The user-data uses `releases/latest/download/xrn-install-linux-amd64`
   - At least one release with that asset attached must exist before the first instance launches

## Usage

1. Edit the configuration block at the top of `create-asg.sh`:
   - `CLUSTER_NAME`, `CLUSTER_REGION`
   - `SATELLITE_REGION`, `SATELLITE_VPC_ID`, `SATELLITE_SUBNET_IDS`, `SATELLITE_NODE_SG`
   - `INSTANCE_PROFILE_NAME`, `INSTANCE_TYPE`
   - ASG sizing (`ASG_MIN_SIZE`, `ASG_MAX_SIZE`, `ASG_DESIRED_CAPACITY`)

2. Run it:
   ```bash
   ./create-asg.sh
   ```

The script:
- Calls `eks:DescribeCluster` to get the API endpoint, CA, and service CIDR
- Looks up the latest AL2023 EKS-optimized AMI for your cluster's K8s version in the satellite region
- Renders the user-data template with discovered values
- Creates (or updates) the launch template
- Creates (or updates) the Auto Scaling group with Cluster Autoscaler discovery tags

## Cluster Autoscaler integration

The ASG is tagged for Cluster Autoscaler discovery:

| Tag | Value | Purpose |
|---|---|---|
| `k8s.io/cluster-autoscaler/<cluster-name>` | `owned` | Identifies this ASG as belonging to the cluster |
| `k8s.io/cluster-autoscaler/enabled` | `true` | Opts the ASG into autoscaler management |
| `k8s.io/cluster-autoscaler/node-template/label/eks.amazonaws.com/compute-type` | `cross-region` | Tells CA what label nodes will have (used for scheduling decisions before the node exists) |
| `k8s.io/cluster-autoscaler/node-template/label/topology.kubernetes.io/region` | `<satellite-region>` | Topology hint |

Cluster Autoscaler must be installed separately. It must have permissions in the satellite region (the ASG is in `eu-west-1`, but CA usually runs in the cluster region). See the [CA cross-region docs](https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler/cloudprovider/aws#auto-discovery-setup).

## Manual scaling

```bash
# Scale up to 2 nodes
aws autoscaling set-desired-capacity --region <satellite-region> \
  --auto-scaling-group-name cross-region-<cluster>-asg \
  --desired-capacity 2

# Scale to zero
aws autoscaling set-desired-capacity --region <satellite-region> \
  --auto-scaling-group-name cross-region-<cluster>-asg \
  --desired-capacity 0
```

## What an instance does at boot

1. cloud-init parses the MIME multipart user-data
2. `nodeadm` (the AMI's systemd service) reads Part 1 (`application/node.eks.aws`) and bootstraps kubelet
3. cloud-init runs Part 2 (`text/x-shellscript`):
   - Downloads `xrn-install-linux-amd64` from `releases/latest/download/`
   - Runs `xrn-install init --cluster-name X --cluster-region Y`
   - `xrn-install` detects nodeadm already ran, skips bootstrap, applies the 5 cross-region kubelet patches, restarts kubelet
4. kubelet registers with the cluster as `system:node:<instance-id>`
5. (Manual) Approve the kubelet-serving CSR — see `../docs/runbook-phase1.md` §9.2

## Troubleshooting

**Instance launches but no Node appears in cluster:**
- SSM into the instance: `aws ssm start-session --region <satellite-region> --target <instance-id>`
- Check cloud-init: `sudo cat /var/log/cloud-init-output.log | tail -100`
- Check kubelet: `sudo journalctl -u kubelet -n 100 --no-pager`
- Check xrn-install ran: `ls -la /usr/local/bin/xrn-install`

**Common failure: GitHub release URL 404.**

If the user-data fails at `curl -sLfo /usr/local/bin/xrn-install ...`, you haven't published a release yet. Tag a release in your fork with `xrn-install-linux-amd64` attached as an asset.
