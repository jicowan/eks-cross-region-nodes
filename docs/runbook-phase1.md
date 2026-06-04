# Phase 1 Runbook: Cross-Region EKS Worker Node (Manual)

**Status:** Draft
**Audience:** Cluster operators with working knowledge of EKS, EC2, VPC, TGW, IAM.
**Time budget:** ~2 hours for a first attempt; ~30 minutes once you've done it.
**Outcome:** One EC2 worker node running in a different VPC and region from the EKS cluster, joined to the cluster, scheduling pods that can reach cluster-region services.

This is the proof-of-concept procedure for [PRD-cross-region-nodes.md](./PRD-cross-region-nodes.md) Phase 1. Everything here is done by hand. Phases 2–4 automate this; you should not be running this runbook for production.

---

## 0. What you are building

```
  Region A (cluster region)              Region B (satellite region)
  ┌──────────────────────────┐           ┌──────────────────────────┐
  │ EKS cluster              │           │                          │
  │ VPC-A: 10.0.0.0/16       │           │ VPC-B: 10.1.0.0/16       │
  │  ├─ subnet 10.0.1.0/24   │           │  ├─ subnet 10.1.1.0/24   │
  │  └─ subnet 10.0.2.0/24   │           │  │   (node primary IP)   │
  │                          │           │  └─ subnet 10.1.10.0/24  │
  │ TGW-A ◄──── peering ────►│ TGW-B     │      (pod IPs via        │
  │                          │           │       ENIConfig)         │
  └──────────────────────────┘           │                          │
                                         │ EC2 GPU instance         │
                                         │  ├─ kubelet              │
                                         │  ├─ aws-node pod         │
                                         │  └─ workload pods        │
                                         └──────────────────────────┘
```

Workloads run in region B. Cross-region traffic is only kubelet ↔ API server and occasional pod ↔ cluster-VPC service.

---

## 1. Prerequisites

You need:

- An existing EKS cluster in region A. Anything from 1.28+ works.
- VPC-A's CIDR block(s) — write them down now, you'll use them several times.
- IAM permissions in **both** the cluster account and the satellite account (if separate) to create VPCs, TGW attachments, IAM roles, EC2 instances.
- `kubectl` configured against the cluster.
- AWS CLI v2 with credentials for both regions.

You also need to have decided:

- **Which region is region B.** Pick one with the GPU instance type you actually want.
- **VPC-B's CIDR.** Must not overlap with VPC-A or any other CIDR you plan to add later. See "CIDR planning" below.
- **The instance type for the worker.** This runbook uses `g5.xlarge` as a placeholder; replace with your real choice.

### CIDR planning

Pick non-overlapping CIDRs now, even if you only intend to add one satellite. The pattern recommended by the parent PRD:

| VPC | CIDR |
|---|---|
| Cluster VPC (region A) | `10.0.0.0/16` |
| Satellite 1 (region B) | `10.1.0.0/16` |
| Satellite 2 (future region C) | `10.2.0.0/16` |

If your existing VPC-A is something else, that's fine — just make sure VPC-B doesn't overlap.

### Choose your endpoint mode

EKS clusters have a public, private, or public+private API endpoint. This affects step 5.

```bash
aws eks describe-cluster \
  --region <region-A> \
  --name <cluster-name> \
  --query 'cluster.resourcesVpcConfig.{endpointPublic:endpointPublicAccess,endpointPrivate:endpointPrivateAccess}'
```

- **Public or public+private:** kubelet from VPC-B can reach the API endpoint over the internet (with NAT GW egress). Easy mode. Continue.
- **Private only:** the cluster's API endpoint hostname only resolves to the private IPs assigned in VPC-A. Cross-region nodes need DNS resolution help. **Stop and read step 5b before you build VPC-B**, because it changes what you put in VPC-B.

---

## 2. Build VPC-B (region B)

In region B, create:

- A VPC with CIDR `10.1.0.0/16`.
- At least two subnets in different AZs:
  - `10.1.1.0/24` (region-B-az-a) — node primary IPs go here.
  - `10.1.10.0/24` (region-B-az-a) — pod IPs go here, via custom networking.
- An internet gateway and a NAT gateway in `10.1.1.0/24`, **only if** the cluster API endpoint is public-only or public+private.
- Route tables — create **explicit** (non-main) route tables and associate them with each subnet, so the routes are easy to audit later:
  - One route table associated with `10.1.1.0/24` (node subnet): default route via NAT GW (or via TGW if you have a centralized egress design); route to `10.0.0.0/16` via TGW (added in step 3).
  - One route table associated with `10.1.10.0/24` (pod subnet): same routes. The pod subnet's route table is what pod egress traffic uses — easy to forget.

Create a security group `sg-vpcb-nodes` in VPC-B with:

- Egress: all traffic to `0.0.0.0/0` (or tighter if you have policy).
- Ingress: TCP 10250 from `10.0.0.0/16` (cluster VPC) — kubelet API, used by EKS control plane for `kubectl exec`/`logs`.
- Ingress: all traffic from itself (node-to-node, pod-to-pod within VPC-B).
- Ingress: all traffic from `10.0.0.0/16` (this is permissive; tighten in production).

Tag everything with `eks-cross-region-poc=true` so you can find and clean it up.

### Update the cluster security group in VPC-A

The EKS cluster security group protects the control-plane ENIs (the API server endpoints). By default it only allows traffic from within VPC-A. Satellite nodes need to reach the API server (TCP 443) and the control plane needs to reach satellite kubelets (TCP 10250). Without this, kubelet requests from VPC-B will time out on connect.

```bash
# Find the cluster security group
CLUSTER_SG=$(aws eks describe-cluster --region <region-A> --name <cluster-name> \
  --query 'cluster.resourcesVpcConfig.clusterSecurityGroupId' --output text)
echo "Cluster SG: $CLUSTER_SG"

# Allow inbound TCP 443 from satellite VPC CIDR (kubelet → API server)
aws ec2 authorize-security-group-ingress --region <region-A> \
  --group-id $CLUSTER_SG \
  --protocol tcp --port 443 \
  --cidr 10.1.0.0/16

# Allow inbound TCP 10250 from cluster VPC to satellite nodes is handled by
# sg-vpcb-nodes above. But the return path (control plane → kubelet) uses the
# cluster SG as source — verify the satellite node SG (sg-vpcb-nodes) allows
# ingress TCP 10250 from the cluster VPC CIDR. We already set that above.
```

> **Why this is easy to miss:** EKS managed node groups in VPC-A don't need this — they share the cluster SG or are auto-configured. Self-managed nodes in a different VPC have no relationship to the cluster SG unless you explicitly add one.

**Verification:**

```bash
aws ec2 describe-security-group-rules --region <region-A> \
  --filters Name=group-id,Values=$CLUSTER_SG \
  --query 'SecurityGroupRules[?IsEgress==`false`].{Port:FromPort,CIDR:CidrIpv4,Description:Description}'
```

You should see `10.1.0.0/16` allowed on port 443.

**VPC-B verification:**

```bash
aws ec2 describe-vpcs --region <region-B> \
  --filters Name=tag:eks-cross-region-poc,Values=true \
  --query 'Vpcs[].{VpcId:VpcId,Cidr:CidrBlock}'
```

You should see one VPC with `10.1.0.0/16`.

---

## 3. Connect VPC-A and VPC-B via TGW

Two reasonable options. Pick one.

### Option A: Two TGWs with peering attachment (recommended)

This is the production-shape option. Each region has its own TGW; they peer. There are **three layers of routing** that all must be configured, and missing any one of them causes a silent black-hole:

1. **VPC route tables** — tell VPC subnets to send cross-VPC traffic to their local TGW.
2. **TGW route tables** — tell the TGW where to forward traffic that arrives from a VPC attachment. For peering, **you must add static routes manually** — TGW peering attachments do NOT propagate routes (VPC attachments can, peering never does).
3. **TGW peering attachment** — the cross-region link itself.

#### Step 3.1: Create TGWs and VPC attachments

In **region A** (cluster region):

```bash
# Create TGW (skip if you have an existing one)
TGW_A=$(aws ec2 create-transit-gateway --region <region-A> \
  --description "Cross-region cluster TGW" \
  --query 'TransitGateway.TransitGatewayId' --output text)
echo "TGW-A: $TGW_A"

# Wait for TGW to become available (~2 min)
aws ec2 wait transit-gateway-available --region <region-A> \
  --transit-gateway-ids $TGW_A

# Attach VPC-A (one subnet per AZ)
VPC_ATTACH_A=$(aws ec2 create-transit-gateway-vpc-attachment --region <region-A> \
  --transit-gateway-id $TGW_A \
  --vpc-id <vpc-a-id> \
  --subnet-ids <subnet-in-az-a> <subnet-in-az-b> \
  --query 'TransitGatewayVpcAttachment.TransitGatewayAttachmentId' --output text)
echo "VPC-A attachment: $VPC_ATTACH_A"
```

In **region B** (satellite region):

```bash
TGW_B=$(aws ec2 create-transit-gateway --region <region-B> \
  --description "Cross-region satellite TGW" \
  --query 'TransitGateway.TransitGatewayId' --output text)
echo "TGW-B: $TGW_B"

aws ec2 wait transit-gateway-available --region <region-B> \
  --transit-gateway-ids $TGW_B

VPC_ATTACH_B=$(aws ec2 create-transit-gateway-vpc-attachment --region <region-B> \
  --transit-gateway-id $TGW_B \
  --vpc-id <vpc-b-id> \
  --subnet-ids <node-subnet> <pod-subnet> \
  --query 'TransitGatewayVpcAttachment.TransitGatewayAttachmentId' --output text)
echo "VPC-B attachment: $VPC_ATTACH_B"
```

#### Step 3.2: Create the TGW peering attachment

Peering is initiated from one side, accepted on the other.

```bash
# Initiate from region A
PEERING_ID=$(aws ec2 create-transit-gateway-peering-attachment --region <region-A> \
  --transit-gateway-id $TGW_A \
  --peer-transit-gateway-id $TGW_B \
  --peer-region <region-B> \
  --query 'TransitGatewayPeeringAttachment.TransitGatewayAttachmentId' --output text)
echo "Peering attachment (region A side): $PEERING_ID"

# Accept from region B (the peering attachment ID is the same, but you query for it in region B)
PEERING_ID_B=$(aws ec2 describe-transit-gateway-peering-attachments --region <region-B> \
  --filters Name=state,Values=pendingAcceptance \
  --query 'TransitGatewayPeeringAttachments[0].TransitGatewayAttachmentId' --output text)

aws ec2 accept-transit-gateway-peering-attachment --region <region-B> \
  --transit-gateway-attachment-id $PEERING_ID_B

# Wait for it to become available (~1-2 min)
echo "Waiting for peering to become available..."
aws ec2 wait transit-gateway-peering-attachment-available --region <region-A> \
  --transit-gateway-attachment-ids $PEERING_ID 2>/dev/null || \
  echo "No waiter available — poll manually:"

aws ec2 describe-transit-gateway-peering-attachments --region <region-A> \
  --transit-gateway-attachment-ids $PEERING_ID \
  --query 'TransitGatewayPeeringAttachments[0].State'
# Must say "available" before proceeding.
```

#### Step 3.3: Add static routes to TGW route tables

This is the step most people miss. Without these routes, traffic enters the TGW from the VPC attachment but the TGW doesn't know to forward it across the peering link.

```bash
# Find the TGW route table in region A
TGW_RTB_A=$(aws ec2 describe-transit-gateway-route-tables --region <region-A> \
  --filters Name=transit-gateway-id,Values=$TGW_A \
  --query 'TransitGatewayRouteTables[0].TransitGatewayRouteTableId' --output text)
echo "TGW-A route table: $TGW_RTB_A"

# Add static route: satellite VPC CIDR → peering attachment
aws ec2 create-transit-gateway-route --region <region-A> \
  --transit-gateway-route-table-id $TGW_RTB_A \
  --destination-cidr-block 10.1.0.0/16 \
  --transit-gateway-attachment-id $PEERING_ID

# Find the TGW route table in region B
TGW_RTB_B=$(aws ec2 describe-transit-gateway-route-tables --region <region-B> \
  --filters Name=transit-gateway-id,Values=$TGW_B \
  --query 'TransitGatewayRouteTables[0].TransitGatewayRouteTableId' --output text)
echo "TGW-B route table: $TGW_RTB_B"

# Add static route: cluster VPC CIDR → peering attachment
aws ec2 create-transit-gateway-route --region <region-B> \
  --transit-gateway-route-table-id $TGW_RTB_B \
  --destination-cidr-block 10.0.0.0/16 \
  --transit-gateway-attachment-id $PEERING_ID_B
```

**Verify the TGW routes are active:**

```bash
# Region A: should show 10.1.0.0/16 → peering, state=active
aws ec2 search-transit-gateway-routes --region <region-A> \
  --transit-gateway-route-table-id $TGW_RTB_A \
  --filters Name=type,Values=static \
  --query 'Routes[].{Cidr:DestinationCidrBlock,State:State,Attachment:TransitGatewayAttachments[0].TransitGatewayAttachmentId}'

# Region B: should show 10.0.0.0/16 → peering, state=active
aws ec2 search-transit-gateway-routes --region <region-B> \
  --transit-gateway-route-table-id $TGW_RTB_B \
  --filters Name=type,Values=static \
  --query 'Routes[].{Cidr:DestinationCidrBlock,State:State,Attachment:TransitGatewayAttachments[0].TransitGatewayAttachmentId}'
```

If `State` is `blackhole`, the peering attachment isn't in `available` state yet — wait and retry.

#### Step 3.4: Add VPC subnet route tables → TGW

This is separate from TGW route tables. VPC route tables tell the *subnets* to send cross-VPC traffic to the TGW in the first place.

In **region A** — update **every VPC route table associated with a subnet that participates in cluster networking** (worker node subnets, plus the subnets used by the EKS control-plane ENIs at cluster creation):

```bash
# Find route tables for VPC-A
aws ec2 describe-route-tables --region <region-A> \
  --filters Name=vpc-id,Values=<vpc-a-id> \
  --query 'RouteTables[].{RT:RouteTableId,Subnets:Associations[?SubnetId!=null].SubnetId,Main:Associations[?Main==`true`].Main|[0]}'

# For EACH route table that has a cluster-networking subnet associated:
aws ec2 create-route --region <region-A> \
  --route-table-id <rtb-xxx> \
  --destination-cidr-block 10.1.0.0/16 \
  --transit-gateway-id $TGW_A
```

In **region B** — update the route tables for **both** the node subnet and the pod subnet. With custom networking, pod ENIs land in the pod subnet, so pod egress to VPC-A uses the pod subnet's route table. Missing this is the most common Phase 1 failure (the node itself can reach VPC-A, but pods cannot):

```bash
# Find route tables for VPC-B
aws ec2 describe-route-tables --region <region-B> \
  --filters Name=vpc-id,Values=<vpc-b-id> \
  --query 'RouteTables[].{RT:RouteTableId,Subnets:Associations[?SubnetId!=null].SubnetId,Main:Associations[?Main==`true`].Main|[0]}'

# For EACH route table (node subnet AND pod subnet):
aws ec2 create-route --region <region-B> \
  --route-table-id <rtb-xxx> \
  --destination-cidr-block 10.0.0.0/16 \
  --transit-gateway-id $TGW_B
```

> A subnet that doesn't appear in any explicit association row is implicitly using the main route table — that's the one case where updating the main table is correct, but it's worth making the association explicit so future you doesn't get confused.

#### Summary: all routes at a glance

| Where | Route table type | Destination | Target | Notes |
|---|---|---|---|---|
| VPC-A subnets | VPC route table | `10.1.0.0/16` | `tgw-a` | One entry per subnet route table |
| TGW-A | TGW route table | `10.1.0.0/16` | peering attachment | **Static — must be added manually** |
| TGW-B | TGW route table | `10.0.0.0/16` | peering attachment | **Static — must be added manually** |
| VPC-B node subnet | VPC route table | `10.0.0.0/16` | `tgw-b` | Node → cluster traffic |
| VPC-B pod subnet | VPC route table | `10.0.0.0/16` | `tgw-b` | Pod → cluster traffic (easy to miss) |

### Option B: Cross-region VPC peering (simpler, less scalable)

VPC peering between VPC-A and VPC-B directly. Update each VPC's route tables to point the other CIDR at the peering connection. No TGW route tables to worry about — peering connections are point-to-point.

This works for a single satellite. It does not scale to multiple satellites without N² peerings.

**Verification (regardless of option):**

From an EC2 instance in VPC-A, ping a host in VPC-B's CIDR (or vice versa). If you don't have a host yet, use `aws ec2 create-network-insights-path` to validate routability:

```bash
# Create a Reachability Analyzer path from a known instance in VPC-A to a hypothetical IP in VPC-B.
# Use --filter-at-source DestinationAddress=... when the destination is an IP that doesn't have a
# resource yet (no instance in VPC-B at this point in the runbook). If you already have an instance
# in VPC-B, use --destination <instance-id> --destination-port 443 instead.
PATH_ID=$(aws ec2 create-network-insights-path \
  --region <region-A> \
  --source <some-instance-id-in-vpc-a> \
  --protocol tcp \
  --filter-at-source 'DestinationAddress=10.1.1.10,DestinationPortRange={FromPort=443,ToPort=443}' \
  --query 'NetworkInsightsPath.NetworkInsightsPathId' --output text)
# The single quotes around --filter-at-source are required: zsh/bash will brace-expand
# {FromPort=443,ToPort=443} into separate args otherwise. Equivalent JSON form:
#   --filter-at-source '{"DestinationAddress":"10.1.1.10","DestinationPortRange":{"FromPort":443,"ToPort":443}}'

ANALYSIS_ID=$(aws ec2 start-network-insights-analysis \
  --region <region-A> \
  --network-insights-path-id "$PATH_ID" \
  --query 'NetworkInsightsAnalysis.NetworkInsightsAnalysisId' --output text)

# Poll until Status != "running" (typically <30s)
```

Wait for status `succeeded` and `NetworkPathFound: true`. If false, the analyzer tells you which hop is missing the route — fix it before continuing.

---

## 4. IAM for the worker node

### Create a separate IAM role for satellite nodes

You **must** use a different IAM role for satellite nodes than for cluster-VPC nodes. This is because:
- Cluster-VPC nodes use `EC2_LINUX` access entries (which validate instance existence in the cluster region).
- Satellite nodes need `HYBRID_LINUX` access entries (which skip that validation).
- Only one access entry type can exist per IAM principal per cluster.

```bash
# Create the trust policy document
cat > /tmp/node-trust-policy.json <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "ec2.amazonaws.com"},
    "Action": "sts:AssumeRole"
  }]
}
EOF

# Create the satellite node role
aws iam create-role \
  --role-name CrossRegionNodeRole \
  --assume-role-policy-document file:///tmp/node-trust-policy.json

# Attach required managed policies
for POLICY in AmazonEKSWorkerNodePolicy AmazonEC2ContainerRegistryReadOnly AmazonEKS_CNI_Policy AmazonSSMManagedInstanceCore; do
  aws iam attach-role-policy \
    --role-name CrossRegionNodeRole \
    --policy-arn arn:aws:iam::aws:policy/$POLICY
done

# Add inline policy for xrn-install pre-flight checks
# (eks:ListAccessEntries / eks:DescribeAccessEntry are NOT in AmazonEKSWorkerNodePolicy)
cat > /tmp/xrn-install-preflight.json <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "eks:ListAccessEntries",
      "eks:DescribeAccessEntry"
    ],
    "Resource": "*"
  }]
}
EOF
aws iam put-role-policy \
  --role-name CrossRegionNodeRole \
  --policy-name AllowXrnInstallPreflight \
  --policy-document file:///tmp/xrn-install-preflight.json

# Create the instance profile and add the role to it
aws iam create-instance-profile \
  --instance-profile-name CrossRegionNodeProfile

aws iam add-role-to-instance-profile \
  --instance-profile-name CrossRegionNodeProfile \
  --role-name CrossRegionNodeRole
```

> **Note:** There's a brief propagation delay (~10–30s) after creating the instance profile before it can be used in `run-instances`. If you get `InvalidParameterValue`, wait and retry.

Policy summary:

| Policy | Why |
|---|---|
| `AmazonEKSWorkerNodePolicy` | kubelet bootstrap, `eks:DescribeCluster` |
| `AmazonEC2ContainerRegistryReadOnly` | Pull container images from ECR |
| `AmazonEKS_CNI_Policy` | aws-node ENI/IP management (`ec2:CreateNetworkInterface`, etc.) |
| `AmazonSSMManagedInstanceCore` | Optional — SSH-less node access via SSM Session Manager |
| `AllowXrnInstallPreflight` (inline) | `eks:ListAccessEntries` + `eks:DescribeAccessEntry` for `xrn-install`'s preflight check that confirms a `HYBRID_LINUX` access entry exists. The check is a warning (not blocking), but the binary still calls these APIs — without the permission you'll see an AccessDenied warning in the install output. |

### Add the role to the EKS cluster's access entries

The access entry must be type **`HYBRID_LINUX`**, not `EC2_LINUX`.

**Why:** `EC2_LINUX` calls `ec2:DescribeInstances` in the cluster's region to verify the instance exists. Your satellite instance is in a *different* region, so this check fails and the API server rejects the token as `Unauthorized`. `HYBRID_LINUX` skips the instance-existence check.

**Important difference:** `HYBRID_LINUX` maps the node identity to `system:node:{{SessionName}}`. For EC2 instance profiles, `{{SessionName}}` is the **instance ID** (e.g., `i-0a5ecec7f33053b35`). This means kubelet must register with `--hostname-override=<instance-id>` — covered in step 7.

```bash
aws eks create-access-entry \
  --region <region-A> \
  --cluster-name <cluster-name> \
  --principal-arn arn:aws:iam::<account-id>:role/CrossRegionNodeRole \
  --type HYBRID_LINUX
```

> **Cluster auth mode must be `API` or `API_AND_CONFIG_MAP`** — only access entries support `HYBRID_LINUX`. If your cluster is on `CONFIG_MAP` only, you cannot use this approach. Note: you cannot switch from `API` back to `API_AND_CONFIG_MAP` (it's a one-way migration).

Verify:

```bash
aws eks list-access-entries --region <region-A> --cluster-name <cluster-name> \
  --query 'accessEntries[?contains(@, `vpc-cni-cross-region-node`)]'
```

---

## 5. Cluster API endpoint reachability from VPC-B

This is the failure mode most likely to bite you. Two cases.

### 5a. Public or public+private endpoint

You're done with this step. The cluster endpoint resolves via public DNS, and the kubelet in VPC-B reaches it through your NAT GW.

Verify from any EC2 instance in VPC-B:

```bash
ENDPOINT=$(aws eks describe-cluster --region <region-A> --name <cluster-name> \
  --query 'cluster.endpoint' --output text)
echo $ENDPOINT
curl -k --max-time 5 ${ENDPOINT}/healthz
```

You should see `ok` and the `curl` should return 200. If DNS fails or the connection times out, fix the NAT GW/IGW/route before continuing.

### 5b. Private endpoint only

The cluster's API endpoint hostname (`*.gr7.<region>.eks.amazonaws.com`) only resolves to private IPs inside VPC-A. From VPC-B, default DNS will fail unless you take one of the steps below.

First, **check whether you actually have a problem** — EKS clusters (especially 1.27+) publish the private endpoint IPs in the public DNS zone (split-horizon DNS). This means the endpoint resolves to private IPs from *any* DNS resolver, including the satellite VPC's default resolver, without any extra configuration.

From any instance in VPC-B (or even your laptop):

```bash
nslookup <your-cluster-endpoint-hostname>
# e.g.: nslookup EEA0CD1B5F844ED46F8AC2D62EAD91D9.gr7.us-east-2.eks.amazonaws.com
```

**If it resolves to private IPs in VPC-A's CIDR range** — you're done. No PHZ association, no resolver needed. Traffic from the satellite VPC will reach those IPs via TGW. Skip ahead to step 6.

**If it returns NXDOMAIN or fails to resolve** — you need a Route53 Resolver setup (Option 1 below).

> **Why can't I just associate the PHZ?** EKS owns the private hosted zone for the cluster endpoint — it's created in an AWS-managed account, not yours. You cannot list it with `route53:ListHostedZonesByVPC` (it won't appear), and you cannot call `route53:CreateVPCAssociationAuthorization` on it (AccessDenied, regardless of your IAM permissions). This is an EKS platform limitation, not an IAM issue.

**Option 1 — Route53 inbound resolver in VPC-A + outbound resolver in VPC-B:**

Use this if the `nslookup` test above failed. This forwards DNS queries for `eks.amazonaws.com` from VPC-B through TGW to VPC-A's resolver, which *can* resolve the PHZ.

In VPC-A:
1. Create a Route53 Resolver inbound endpoint with two ENIs in two AZs of VPC-A.
2. Note the inbound endpoint's IP addresses (two private IPs in VPC-A).

```bash
# Create inbound endpoint in VPC-A
INBOUND_ID=$(aws route53resolver create-resolver-endpoint --region <region-A> \
  --creator-request-id "cross-region-inbound-$(date +%s)" \
  --direction INBOUND \
  --security-group-ids <sg-that-allows-tcp-udp-53-from-vpc-b-cidr> \
  --ip-addresses SubnetId=<vpc-a-subnet-az-a> SubnetId=<vpc-a-subnet-az-b> \
  --query 'ResolverEndpoint.Id' --output text)

# Get the resolver IPs (these go into VPC-B's forwarding rule)
aws route53resolver list-resolver-endpoint-ip-addresses --region <region-A> \
  --resolver-endpoint-id $INBOUND_ID \
  --query 'IpAddresses[].Ip'
```

In VPC-B:
1. Create a Route53 Resolver outbound endpoint with two ENIs in two AZs of VPC-B.
2. Create a forwarding rule: domain `eks.amazonaws.com`, target IPs = the inbound endpoint IPs from VPC-A. Associate the rule with VPC-B.

```bash
# Create outbound endpoint in VPC-B
OUTBOUND_ID=$(aws route53resolver create-resolver-endpoint --region <region-B> \
  --creator-request-id "cross-region-outbound-$(date +%s)" \
  --direction OUTBOUND \
  --security-group-ids <sg-that-allows-all-outbound> \
  --ip-addresses SubnetId=<vpc-b-subnet-az-a> SubnetId=<vpc-b-subnet-az-b> \
  --query 'ResolverEndpoint.Id' --output text)

# Create forwarding rule — forward eks.amazonaws.com to VPC-A's inbound resolver
aws route53resolver create-resolver-rule --region <region-B> \
  --creator-request-id "cross-region-fwd-$(date +%s)" \
  --rule-type FORWARD \
  --domain-name eks.amazonaws.com \
  --resolver-endpoint-id $OUTBOUND_ID \
  --target-ips "Ip=<inbound-ip-1>,Port=53" "Ip=<inbound-ip-2>,Port=53" \
  --name "forward-eks-to-cluster-vpc"

# Associate the rule with VPC-B
RULE_ID=$(aws route53resolver list-resolver-rules --region <region-B> \
  --query "ResolverRules[?Name=='forward-eks-to-cluster-vpc'].Id" --output text)
aws route53resolver associate-resolver-rule --region <region-B> \
  --resolver-rule-id $RULE_ID \
  --vpc-id <vpc-b-id>
```

Verify from an EC2 instance in VPC-B:

```bash
# Should now resolve to private IPs in VPC-A's range
nslookup ${ENDPOINT#https://}
```

> **Note:** The inbound endpoint's security group must allow TCP/UDP 53 from VPC-B's CIDR (traffic arrives via TGW). The outbound endpoint just needs outbound access. Resolver endpoints cost ~$0.125/hr per ENI (4 ENIs total = ~$365/month). For production, this is negligible; for a PoC, it's worth knowing.

**Option 2 — Switch the cluster to public+private endpoint:**

If you don't want to run resolver infrastructure and the `nslookup` test failed, the simplest workaround is to enable public access on the cluster endpoint (in addition to private). Satellite nodes reach the API via internet, cluster-VPC nodes continue using the private path. This is less secure but zero infrastructure:

```bash
aws eks update-cluster-config --region <region-A> --name <cluster-name> \
  --resources-vpc-config endpointPublicAccess=true,endpointPrivateAccess=true
```

You can restrict public access to specific CIDRs (e.g., the NAT GW EIPs of your satellite VPCs):

```bash
aws eks update-cluster-config --region <region-A> --name <cluster-name> \
  --resources-vpc-config endpointPublicAccess=true,publicAccessCidrs=<nat-gw-eip-1>/32,<nat-gw-eip-2>/32
```

---

## 6. Set up custom networking on the cluster

This is what makes pods get IPs from VPC-B's pod subnet instead of trying (and failing) to use VPC-A IPs.

### 6.1. Enable custom networking on the aws-node DaemonSet

```bash
kubectl set env daemonset aws-node -n kube-system \
  AWS_VPC_K8S_CNI_CUSTOM_NETWORK_CFG=true \
  ENI_CONFIG_LABEL_DEF=topology.kubernetes.io/zone
```

This tells aws-node:
- Use `ENIConfig` CRs instead of the node's primary VPC/subnet for pod IPs.
- Look up the `ENIConfig` by the node's `topology.kubernetes.io/zone` label.

Wait for the rollout:

```bash
kubectl rollout status daemonset aws-node -n kube-system
```

### 6.2. Create the `ENIConfig` for region B's pod subnet

```yaml
# Save as eniconfig-region-b.yaml
apiVersion: crd.k8s.amazonaws.com/v1alpha1
kind: ENIConfig
metadata:
  # Name MUST match the AZ name. ENI_CONFIG_LABEL_DEF=topology.kubernetes.io/zone
  # tells aws-node to look up the ENIConfig whose name equals the node's zone.
  name: eu-west-1b   # e.g., us-west-2a
spec:
  subnet: subnet-02831de2565a4f110
  securityGroups:
    - sg-04a0cbd3f55689742
```

```bash
kubectl apply -f eniconfig-region-b.yaml
```

Repeat for every AZ in region B you'll launch nodes into.

### 6.3. Set the manual SNAT exclusion

This is the Phase 1 manual step that Phase 2 automates. The SNAT exclusion list must include **all VPC CIDRs across the entire cluster** — both the cluster VPC and every satellite VPC. This ensures:

- Pods in VPC-B don't SNAT traffic to VPC-A (preserves source IP toward cluster services).
- Pods in VPC-A don't SNAT traffic to VPC-B (preserves source IP toward satellite pods).

Without both directions, cross-VPC pod-to-pod traffic has the source IP rewritten to the node's primary IP, which breaks NetworkPolicy, audit logging, and anything source-IP-aware.

```bash
kubectl set env daemonset aws-node -n kube-system \
  AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS=10.0.0.0/16,10.1.0.0/16
```

Include all CIDRs — cluster VPC + all satellite VPCs. If you have secondary CIDRs (e.g., `100.64.0.0/16` for custom networking), include those too:

```bash
kubectl set env daemonset aws-node -n kube-system \
  AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS=10.0.0.0/16,10.1.0.0/16,100.64.0.0/16
```

Wait for the rollout:

```bash
kubectl rollout status daemonset aws-node -n kube-system
```

> **Note:** This env var is set on the DaemonSet, so it applies to aws-node on *every* node (VPC-A and VPC-B). That's intentional — both sides need to know about both CIDRs. On VPC-A nodes, the cluster VPC CIDR is already excluded via IMDS-derived VPC CIDRs — adding it again via env is redundant but harmless (deduplicated in the iptables rules). The satellite CIDR is the one that's actually new to VPC-A nodes.

---

## 7. Launch the satellite worker node

### 7.1. Pick an AMI

In region B, look up an EKS-optimized AMI for your cluster's Kubernetes version:

```bash
aws ssm get-parameter --region <region-B> \
  --name /aws/service/eks/optimized-ami/<k8s-version>/amazon-linux-2023/x86_64/standard/recommended/image_id \
  --query 'Parameter.Value' --output text
```

For GPU nodes, replace `standard` with `nvidia` in the parameter path.

### 7.2. Build user-data

On AL2023 EKS AMIs, `nodeadm` is the bootstrap mechanism. It looks for a `NodeConfig` document with content type `application/node.eks.aws` in IMDS user-data. The challenge: nodeadm generates kubelet configuration that is **wrong for cross-region nodes** in several ways:

- Sets `--cloud-provider=external` → must be empty (otherwise CCM deletes the node)
- Sets `--hostname-override=<private-dns-name>` → must be instance ID (to match `HYBRID_LINUX` identity)
- Sets `providerID=aws:///<az>/<instance-id>` → must be `eks-hybrid:///<cluster-region>/<cluster-name>/<instance-id>`
- Sets `--region=<node-region>` in kubeconfig → must be cluster region (for valid STS token)
- Doesn't add topology labels → must be set manually (no CCM to do it)

The solution: a **two-part MIME multipart** user-data. Part 1 runs nodeadm (bootstrap). Part 2 is a shell script that patches the generated config and restarts kubelet.

First, get your cluster's details:

```bash
aws eks describe-cluster --region <region-A> --name <cluster-name> \
  --query 'cluster.{endpoint:endpoint,ca:certificateAuthority.data,cidr:kubernetesNetworkConfig.serviceIpv4Cidr}' \
  --output json
```

Save as `userdata.txt` (replace all `<placeholder>` values):

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
    region: <region-A>
    apiServerEndpoint: <endpoint-from-describe-cluster>
    certificateAuthority: <base64-CA-from-describe-cluster>
    cidr: <service-cidr>
  kubelet:
    flags:
      - --node-labels=eks.amazonaws.com/compute-type=cross-region

--BOUNDARY
Content-Type: text/x-shellscript

#!/bin/bash
# Post-bootstrap fixup for cross-region nodes.
# This runs AFTER nodeadm completes, patches the kubelet config, and restarts kubelet.

set -euo pipefail

CLUSTER_REGION="<region-A>"
CLUSTER_NAME="<cluster-name>"

# Get instance metadata
TOKEN=$(curl -sX PUT http://169.254.169.254/latest/api/token -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/instance-id)
AZ=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/availability-zone)
NODE_REGION=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/placement/region)

# 1. Fix cloud-provider: must be empty, not "external"
#    (external triggers CCM to validate instance in cluster region → deletes the node)
sed -i 's/--cloud-provider=external/--cloud-provider=/' /etc/eks/kubelet/environment

# 2. Fix hostname-override: must be instance ID (matches HYBRID_LINUX system:node:{{SessionName}})
sed -i "s/--hostname-override=[^ ]*/--hostname-override=$INSTANCE_ID/" /etc/eks/kubelet/environment

# 3. Add topology labels (no CCM to do this when cloud-provider is empty)
sed -i "s/--node-labels=\([^ ]*\)/--node-labels=\1,topology.kubernetes.io\/zone=$AZ,topology.kubernetes.io\/region=$NODE_REGION/" /etc/eks/kubelet/environment

# 4. Fix providerID: must use eks-hybrid:/// prefix (not aws:///)
sed -i "s|\"providerID\": \"aws:///[^\"]*\"|\"providerID\": \"eks-hybrid:///$CLUSTER_REGION/$CLUSTER_NAME/$INSTANCE_ID\"|" /etc/kubernetes/kubelet/config.json

# 5. Fix kubeconfig region: token must be scoped to cluster region, not node region
sed -i "s/\"$NODE_REGION\"/\"$CLUSTER_REGION\"/" /var/lib/kubelet/kubeconfig

# Restart kubelet with corrected config
systemctl daemon-reload
systemctl restart kubelet

--BOUNDARY--
```

> **Key points:**
> - `Content-Type: application/node.eks.aws` runs nodeadm. `Content-Type: text/x-shellscript` runs after nodeadm completes via cloud-init.
> - The shell script patches 5 things that nodeadm gets wrong for cross-region: cloud-provider, hostname, topology labels, providerID, and kubeconfig region.
> - The MIME boundary (`BOUNDARY`) must match exactly between header and delimiters.
> - The `--node-labels` in the NodeConfig part sets `compute-type=cross-region`; the shell script appends the topology labels to whatever nodeadm wrote.

### 7.3. Launch the instance

```bash
aws ec2 run-instances --region <region-B> \
  --image-id <ami-id-from-7.1> \
  --instance-type g5.xlarge \
  --iam-instance-profile Name=CrossRegionNodeProfile \
  --subnet-id <node-primary-subnet-in-region-B> \
  --security-group-ids sg-vpcb-nodes \
  --user-data file://userdata.txt \
  --metadata-options "HttpTokens=required,HttpPutResponseHopLimit=2,HttpEndpoint=enabled" \
  --tag-specifications 'ResourceType=instance,Tags=[{Key=eks-cross-region-poc,Value=true},{Key=Name,Value=cross-region-gpu-1}]'
```

> **`HttpPutResponseHopLimit=2` is mandatory.** Without it, pods cannot reach IMDS (because the request crosses one extra hop through the pod's network namespace) and aws-node will fail to start.

### 7.4. Watch it join

From your kubectl context:

```bash
kubectl get nodes -w
```

The node will appear with its **instance ID** as the name (e.g., `i-0a5ecec7f33053b35`), not a DNS hostname. This is expected — `HYBRID_LINUX` maps identity to the instance ID, and `--hostname-override` must match.

It should reach `Ready` in 60–120 seconds (nodeadm bootstrap + fixup script + kubelet restart + CNI initialization).

If it doesn't appear within 5 minutes, jump to **Troubleshooting** below.

---

## 8. Verification

### 8.1. Node is healthy and labeled correctly

```bash
NODE=$(kubectl get nodes -l eks.amazonaws.com/compute-type=cross-region \
  -o jsonpath='{.items[0].metadata.name}')

kubectl describe node $NODE | grep -E "(Labels|Conditions|Taints)" -A 5
```

Verify:
- `Ready=True`
- Labels include `topology.kubernetes.io/zone=<region-B>a` and `eks.amazonaws.com/compute-type=cross-region`.
- No unexpected taints.

### 8.2. aws-node pod is healthy on the satellite node

```bash
kubectl get pods -n kube-system -o wide | grep $NODE
```

The aws-node pod should be `Running 1/1`. Check its logs for `INFO` not `ERROR`:

```bash
kubectl logs -n kube-system -l k8s-app=aws-node --field-selector spec.nodeName=$NODE --tail=50
```

Look for:
- `Using ENI Config: <region-B>a` — confirms custom networking picked up the right ENIConfig.
- `Setting up host network` followed by no errors.
- No retries against the EC2 API.

### 8.3. Pod gets an IP in the pod subnet

```bash
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: cross-region-canary
spec:
  nodeSelector:
    eks.amazonaws.com/compute-type: cross-region
  tolerations:
  - operator: Exists
  containers:
  - name: c
    image: public.ecr.aws/amazonlinux/amazonlinux:2023
    command: ["sleep", "3600"]
EOF

kubectl wait --for=condition=Ready pod/cross-region-canary --timeout=60s
kubectl get pod cross-region-canary -o jsonpath='{.status.podIP}'
echo
```

The pod IP must be in the pod subnet you put in the `ENIConfig` (e.g., `10.1.10.x`), **not** the node subnet (`10.1.1.x`) and **not** the cluster VPC range (`10.0.x.x`).

### 8.4. Pod can reach a cluster-region Service

```bash
kubectl exec cross-region-canary -- curl -sk --max-time 5 https://kubernetes.default.svc/healthz
```

Expect `ok`. This proves:
- DNS works inside the pod (CoreDNS resolution from VPC-B → cluster-VPC CoreDNS pods).
- TGW routing works (the kubernetes Service ClusterIP is in the service CIDR, and packets to it route via the cluster VPC).

### 8.5. Source IP is preserved

This is the test that validates the manual SNAT exclusion (step 6.3).

On a node in **VPC-A**, run a netcat listener:

```bash
# On any cluster-VPC node, via SSH or SSM
nc -l -p 9999
```

From the cross-region pod:

```bash
kubectl exec cross-region-canary -- sh -c \
  'echo "from-pod-$(hostname)" | nc -w 2 <vpc-a-node-private-ip> 9999'
```

On the listener side, you should see the `from-pod-...` message. Now check the *source IP* on the listener:

```bash
# On the listener side, while nc is still receiving:
ss -tn 'sport = :9999'
```

The peer IP must be the pod IP (`10.1.10.x`). If it's the *node's* primary IP (`10.1.1.x`), SNAT exclusion is misconfigured — go back and verify step 6.3 took effect:

```bash
kubectl exec -n kube-system $(kubectl get pod -n kube-system \
  -l k8s-app=aws-node --field-selector spec.nodeName=$NODE \
  -o jsonpath='{.items[0].metadata.name}') -- env | grep EXCLUDE_SNAT
```

### 8.6. Brief disconnect tolerance

Optional but worth checking before declaring victory. Briefly drop the cross-region path (e.g., temporarily remove the TGW route) and confirm:

- The node does not flap to `NotReady` immediately (kubelet has its own reconnect backoff, ~40s).
- Pods continue running.
- Restoring the route reconnects within 30s without pod restarts.

Skip this if you can't safely break the route.

---

## 9. Enable `kubectl logs` and `kubectl exec`

After §8 passes, the node is `Ready` and pods run correctly, but `kubectl logs <pod-on-satellite>` and `kubectl exec` will time out. Two extra steps are required to enable the control plane → kubelet path.

### 9.1. Set `RemoteNetworkConfig` on the cluster

By default the EKS control plane only routes to nodes inside the cluster VPC. Tell it the satellite CIDR is reachable:

```bash
aws eks update-cluster-config --region <region-A> --name <cluster-name> \
  --remote-network-config '{
    "remoteNodeNetworks": [{"cidrs": ["10.1.0.0/16"]}],
    "remotePodNetworks":  [{"cidrs": ["10.1.0.0/16"]}]
  }'

# Wait for the cluster to return to ACTIVE
aws eks describe-cluster --region <region-A> --name <cluster-name> \
  --query 'cluster.status' --output text
```

> **Heads up:** updating `RemoteNetworkConfig` on a cluster that already has satellite nodes joined causes the existing Node objects to be deleted. Kubelet does NOT auto-re-register; it gets stuck PATCH'ing a missing Node. After the cluster returns to `ACTIVE`, **restart kubelet on each satellite node** to re-register:
>
> ```bash
> aws ssm send-command --region <region-B> \
>   --instance-ids <instance-id> \
>   --document-name "AWS-RunShellScript" \
>   --parameters 'commands=["systemctl restart kubelet"]'
> ```
>
> This is one-time per node. Once you set `RemoteNetworkConfig` *before* joining nodes, this isn't needed.

### 9.2. Approve kubelet-serving CSRs

EKS-optimized AMIs set `serverTLSBootstrap: true`. Kubelet generates a `kubernetes.io/kubelet-serving` CSR and waits for approval before serving HTTPS on port 10250 (the port the control plane uses for `logs`/`exec`/`port-forward`).

EKS auto-approves CSRs from cluster-VPC nodes via internal mechanisms but **does not auto-approve CSRs from cross-region nodes** (verified — the CSRs sit `Pending` indefinitely even with `RemoteNetworkConfig` set).

#### Manual approval (one-shot)

```bash
# Approve all pending CSRs from a specific satellite node
NODE_NAME=<instance-id>  # e.g., i-0a5ecec7f33053b35
kubectl get csr -o json | \
  jq -r ".items[] | select(.spec.username==\"system:node:$NODE_NAME\") | select(.status.conditions==null or (.status.conditions|length)==0) | .metadata.name" | \
  xargs -I {} kubectl certificate approve {}
```

The cert is valid ~9 months. Kubelet rotates it earlier (~80% of TTL), so manual approval recurs.

#### Permanent: deploy a serving cert auto-approver

For long-running clusters, install [`kubelet-serving-cert-approver`](https://github.com/alex1989hu/kubelet-serving-cert-approver):

```bash
helm repo add kubelet-serving-cert-approver https://alex1989hu.github.io/kubelet-serving-cert-approver/
helm install kubelet-serving-cert-approver kubelet-serving-cert-approver/kubelet-serving-cert-approver \
  --namespace kubelet-serving-cert-approver --create-namespace
```

It validates each CSR's SANs against the requesting Node's `status.addresses` before approving. Works for both cluster-VPC and satellite nodes.

### 9.3. Verify

```bash
# This should now succeed (was timing out before §9)
kubectl logs cross-region-canary

# Same for exec
kubectl exec -it cross-region-canary -- /bin/sh
```

### Why both fixes are needed

| Symptom | Root cause | Fix |
|---|---|---|
| TCP connection to kubelet:10250 hangs (no SYN-ACK) | Control plane has no route to satellite CIDR | §9.1 `RemoteNetworkConfig` |
| TCP connects but TLS handshake fails (`no serving certificate available`) | Kubelet has no approved serving cert | §9.2 CSR approval |

You need both — `RemoteNetworkConfig` alone leaves you with TLS errors; CSR approval alone leaves you with connection timeouts.

### What works without §9

A satellite node without §9 still:
- Registers and shows `Ready`
- Runs pods
- Networks pods (pod IPs, DNS, service connectivity all work)
- Routes pod traffic via TGW

What breaks without §9: `kubectl logs/exec/port-forward` for pods on the satellite node, and `kubectl top node/pod` for the satellite (metrics-server scrapes via 10250).

---

## 10. Cleanup

```bash
# Delete the canary pod
kubectl delete pod cross-region-canary

# Terminate the satellite node
aws ec2 terminate-instances --region <region-B> \
  --instance-ids <instance-id>

# Wait for node to leave the cluster, then remove the ENIConfig
kubectl delete eniconfig <region-B>a

# Revert SNAT exclusion if you don't intend to keep cross-region nodes
kubectl set env daemonset aws-node -n kube-system AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS-

# Revert custom networking (only if no other satellite or custom-networking deployment)
kubectl set env daemonset aws-node -n kube-system \
  AWS_VPC_K8S_CNI_CUSTOM_NETWORK_CFG=false ENI_CONFIG_LABEL_DEF-
```

Tear down VPC-B, TGW peering, IAM role/access-entry on your own schedule.

---

## Troubleshooting

### Node never appears in `kubectl get nodes`

In order, check:

1. **EC2 instance is running and reachable on SSM:**
   ```bash
   aws ssm start-session --region <region-B> --target <instance-id>
   ```
2. **Cloud-init finished without errors:**
   ```bash
   sudo cat /var/log/cloud-init-output.log | tail -100
   ```
   Look for `nodeadm init` failures, AWS CLI errors, missing jq, etc.
3. **kubelet is running:**
   ```bash
   sudo systemctl status kubelet
   sudo journalctl -u kubelet -n 200 --no-pager
   ```
   Common errors:
   - `failed to get node` → IAM access entry missing (step 4) or wrong role.
   - `connection refused` / `i/o timeout` to the API endpoint → step 5 misconfigured (DNS or routing).
4. **API endpoint resolution from the node:**
   ```bash
   ENDPOINT=$(aws eks describe-cluster --region <region-A> --name <cluster> \
     --query 'cluster.endpoint' --output text)
   nslookup ${ENDPOINT#https://}
   curl -k --max-time 5 ${ENDPOINT}/healthz
   ```

### Node `Ready` but pods stay `ContainerCreating`

Almost always an IMDS hop limit problem. From the satellite node:

```bash
sudo crictl ps  # find aws-node container
sudo crictl logs <aws-node-container-id> 2>&1 | head -50
```

Look for IMDS timeout errors. Fix:

```bash
aws ec2 modify-instance-metadata-options --region <region-B> \
  --instance-id <instance-id> \
  --http-put-response-hop-limit 2 \
  --http-tokens required
```

Then reboot the instance (the existing aws-node pod won't pick up new IMDS settings).

### Pod gets an IP but can't reach cluster services

In order:

1. **Verify the pod IP is in the pod subnet:** see 8.3.
2. **Verify the SNAT exclusion is set:** see end of 8.5.
3. **Verify TGW routes exist in both directions, on the right route tables.** The most common miss: the pod subnet's route table doesn't have the route to VPC-A. The node itself can reach VPC-A (its primary IP is in the node subnet, whose route table is fine), but pod ENIs are in the pod subnet — pod egress uses that subnet's route table. Check:
   ```bash
   # The pod subnet's route table must include 10.0.0.0/16 → tgw-b
   POD_SUBNET=<10.1.10.0/24 subnet id>
   aws ec2 describe-route-tables --region <region-B> \
     --filters Name=association.subnet-id,Values=$POD_SUBNET \
     --query 'RouteTables[].Routes[?DestinationCidrBlock==`10.0.0.0/16`]'
   ```
   Also verify: route from VPC-A to VPC-B in every cluster-side subnet's route table; both directions in TGW route tables.
4. **Verify cluster security group allows ingress from VPC-B's CIDR.** This is the cluster's "additional security group" or the EKS-managed cluster SG. Add ingress for `10.1.0.0/16` if missing.
5. **Reachability Analyzer:** create a path from the satellite EC2 instance to the cluster's API endpoint IP (or any IP in VPC-A) and start an analysis. This will tell you exactly which hop is dropping the packet.

### Source IP not preserved (step 8.5 fails)

`AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS` either isn't set on the DaemonSet or didn't roll out to the satellite node. Check:

```bash
kubectl get daemonset aws-node -n kube-system -o yaml | grep -A 2 EXCLUDE_SNAT
kubectl exec -n kube-system <aws-node-pod-on-satellite> -- iptables -t nat -L AWS-SNAT-CHAIN-0 -n
```

The iptables chain should have a rule excluding `10.0.0.0/16` *before* the SNAT rule.

If it's missing, restart the aws-node pod on the satellite node:

```bash
kubectl delete pod -n kube-system <aws-node-pod-on-satellite>
```

Wait for it to recreate, re-run 8.5.

### `kubectl exec` / `kubectl logs` fails against the satellite pod

The control plane reaches kubelet on TCP 10250 over the cluster ENIs. Verify:

- `sg-vpcb-nodes` allows ingress on TCP 10250 from VPC-A's CIDR (step 2).
- TGW route from VPC-A to VPC-B exists (step 3).
- The node's primary IP is reachable from VPC-A (Reachability Analyzer).

This is independent of pod-to-pod connectivity — it's a separate flow that hits the node's primary IP, not pod IPs.

### Node appears briefly then disappears

**Cause:** `--cloud-provider=external` is set (or `--cloud-provider` is not explicitly empty). The EKS cloud controller manager (CCM) calls `ec2:DescribeInstances` in the cluster's region, doesn't find the satellite instance, and deletes the Node object. This typically happens within 30–60 seconds of the node registering.

**Verified:** Setting `providerID=eks-hybrid:///...` alone is NOT sufficient. The CCM still deletes the node if `--cloud-provider=external` is set. Only `--cloud-provider=""` (empty string) prevents CCM from managing the node.

**Fix:**
```bash
# On the satellite node:
sed -i 's/--cloud-provider=external/--cloud-provider=/' /etc/eks/kubelet/environment
systemctl daemon-reload
systemctl restart kubelet
```

### "Unauthorized" after network connectivity works (curl healthz returns ok)

**Cause:** The kubeconfig's `aws eks get-token --region` is set to the node's region (e.g., `eu-west-1`) instead of the cluster region (e.g., `us-east-2`). The token is signed against the wrong STS endpoint, and EKS rejects it.

**Fix:**
```bash
# On the satellite node — change the region in the kubeconfig:
sed -i 's/"<node-region>"/"<cluster-region>"/' /var/lib/kubelet/kubeconfig
systemctl restart kubelet
```

### "cannot get resource csinodes" / "can only access CSINode with the same name as the requesting node"

**Cause:** The `--hostname-override` doesn't match the `HYBRID_LINUX` identity. `HYBRID_LINUX` maps the node identity to `system:node:<instance-id>` (the STS session name). But kubelet registered with a DNS hostname. The Node authorizer enforces that `system:node:X` can only access node `X`.

**Fix:**
```bash
# On the satellite node:
INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $(curl -sX PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')" http://169.254.169.254/latest/meta-data/instance-id)
sed -i "s/--hostname-override=[^ ]*/--hostname-override=$INSTANCE_ID/" /etc/eks/kubelet/environment
systemctl daemon-reload
systemctl restart kubelet
```

### ENIConfig not found / "No ENIConfig could be found for this node"

**Cause:** The node doesn't have a `topology.kubernetes.io/zone` label. With `--cloud-provider=""`, no controller adds topology labels automatically. IPAMD looks up the ENIConfig by the value of `ENI_CONFIG_LABEL_DEF` (which is `topology.kubernetes.io/zone`), finds nothing, falls back to `"default"`, and fails.

**Fix:**
```bash
# On the satellite node — add topology labels:
AZ=$(curl -s -H "X-aws-ec2-metadata-token: $(curl -sX PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')" http://169.254.169.254/latest/meta-data/placement/availability-zone)
REGION=$(curl -s -H "X-aws-ec2-metadata-token: $(curl -sX PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')" http://169.254.169.254/latest/meta-data/placement/region)
sed -i "s/--node-labels=\([^ ]*\)/--node-labels=\1,topology.kubernetes.io\/zone=$AZ,topology.kubernetes.io\/region=$REGION/" /etc/eks/kubelet/environment
systemctl daemon-reload
systemctl restart kubelet
```

Also verify an ENIConfig CR exists whose name matches the AZ (e.g., `eu-west-1b`).

---

## What's next

If everything in §8 passes: the topology works. You're ready for Phase 2 (the ConfigMap watcher), which lets you change `EXCLUDE_SNAT_CIDRS` cluster-wide without a DaemonSet rollout. See [PRD-configmap-snat-watcher.md](./PRD-configmap-snat-watcher.md).

If you're planning to run more than one satellite, Phase 3 (the installer) and Phase 4 (`xrnctl`) automate everything in this runbook except the customer-underlay parts (TGW, IAM access entries, security groups). See [PRD-cross-region-nodes.md](./PRD-cross-region-nodes.md).
