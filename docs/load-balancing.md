# Load balancing for satellite-node workloads

How to expose pods that run on cross-region / cross-account satellite nodes through an AWS load
balancer, using the AWS Load Balancer Controller (LBC).

> **Status:** design analysis, reasoned from the official AWS Load Balancer Controller documentation
> and the `TargetGroupBinding` CRD source (reviewed for this writeup) — not lab-validated the way the
> VPC CNI path was. The mechanisms cited (`TargetGroupBinding` cross-account fields, multi-cluster
> mode, `ip` target CIDR rules) are real and documented; the end-to-end against a satellite node has
> not been exercised here.

## TL;DR

- A **single LBC deployment is scoped to one region + one VPC + one account.** It owns the load
  balancers it creates and the worker-node security-group rules in *its* VPC. There is one LBC
  deployment per cluster.
- The **automatic path** (`Service type: LoadBalancer` or `Ingress` with annotations, where the LBC
  *creates* the ALB/NLB) is effectively **single-VPC**: it assumes pod ENIs and cluster-tagged
  security groups in the controller's own VPC. It does **not** fit pods on a satellite node in a
  different VPC/account.
- The **`TargetGroupBinding` path** is the cross-VPC / cross-account mechanism. You (or another tool)
  pre-create the load balancer + target group; the LBC only **registers pod IPs** into it. The CRD
  has first-class fields for a target group in **another VPC** (`vpcID`) and **another account**
  (`iamRoleArnToAssume` + `assumeRoleExternalId`).
- **Target type must be `ip`** (pod IPs), and those IPs must be **routable to the load balancer**
  (over TGW/peering) and in the allowed private ranges. Our VPC-native satellite pod IPs satisfy this.

This makes the LBC the **only** AWS controller in this design with a purpose-built cross-account
mechanism — unlike the EBS CSI controller (needs a forked, renamed driver) or Cluster Autoscaler
(needs a per-region instance).

## 1. What a single controller instance can and can't do

From the LBC configuration docs, one controller deployment is bound to a single scope:

- `--aws-region` — the region it calls ELB/EC2 APIs in (defaults to instance metadata).
- `--aws-vpc-id` — the VPC it operates in (defaults to instance metadata).
- `--cluster-name` — mandatory; drives subnet/SG auto-discovery via `kubernetes.io/cluster/<name>` tags.
- Only **one** controller deployment per cluster is supported (v2.0.0+), and it "assumes it's the
  solo owner of worker node security group rules."

So anything the controller **creates or mutates itself** — load balancers, listeners, the LB
security group, worker-node SG ingress rules — happens in *that* region/VPC/account. You can't point
one controller at two regions, and running a second controller for a satellite region is not a
supported topology.

## 2. The automatic path (LBC creates the LB) — single-VPC only

The familiar usage — a `Service` of `type: LoadBalancer` with
`service.beta.kubernetes.io/aws-load-balancer-type: external` +
`...nlb-target-type: ip`, or an `Ingress` with `alb.ingress.kubernetes.io/*` annotations — has the
controller **provision** the NLB/ALB, discover subnets by cluster tag, and manage the backend
security groups.

For `ip` target type this path assumes:

- pods have native VPC networking (VPC CNI) **in the controller's VPC**,
- pod ENIs carry the cluster's security-group tags,
- the LB and targets share the controller's VPC/subnets (selected by `kubernetes.io/cluster/<name>` tags).

Those assumptions don't hold for a pod on a satellite node in a **different** VPC/account. Use this
path for cluster-VPC workloads; do **not** rely on it to expose satellite pods.

## 3. The `TargetGroupBinding` path — cross-VPC and cross-account

`TargetGroupBinding` (TGB) decouples **load-balancer provisioning** from **target registration**.
You provision the ALB/NLB + target group + listener outside Kubernetes (Terraform, CLI, CloudFormation);
the LBC watches a Service and **only** registers/deregisters pod endpoints into the existing target
group. The controller does **not** create or delete the load balancer.

AWS calls the controller makes for a TGB: `RegisterTargets`, `DeregisterTargets`,
`DescribeTargetGroups`, `DescribeTargetHealth`. That's the entire API surface — no
`CreateLoadBalancer`, no LB security-group creation.

### 3.1 The fields that make it cross-VPC / cross-account

From the `TargetGroupBinding` CRD spec:

| Field | Purpose |
|---|---|
| `targetGroupARN` (or `targetGroupName`) | the pre-created target group to manage |
| `targetType: ip` | register **pod IPs** (required for cross-VPC; `instance` requires same-VPC nodes) |
| `vpcID` | *"VPC of the TargetGroup. If unspecified, automatically inferred."* — set this when the target group's VPC differs from the controller's |
| `iamRoleArnToAssume` | *"IAM Role ARN to assume when calling AWS APIs. Useful if the target group is in a different AWS account."* |
| `assumeRoleExternalId` | external id for that AssumeRole — *"Needed to assume a role in another account and prevent the confused deputy problem."* |
| `serviceRef` | the Kubernetes Service whose endpoints are registered |
| `networking` | ingress rules allowing the LB to reach the targets |

So a TGB in the cluster can manage a target group that lives in a **different VPC** (`vpcID`) and a
**different account** (`iamRoleArnToAssume` + `assumeRoleExternalId`), registering satellite pod IPs
into it. This is the seam to use for satellite workloads.

### 3.2 Recommended pattern for satellite pods

1. **Provision the load balancer + target group** (target type `ip`) yourself, in the account/VPC
   where you want the LB to live. For a satellite in account B you'd typically create the LB and TG
   in account B's satellite VPC; for a cluster-region LB fronting satellite pods, create them in the
   cluster VPC.
2. **Create a `TargetGroupBinding`** in the cluster referencing that target group's ARN, with
   `targetType: ip`, `vpcID` set to the target group's VPC, and (if the TG is in another account)
   `iamRoleArnToAssume` + `assumeRoleExternalId`.
3. The LBC registers the **satellite pod IPs** as targets. Ensure those IPs are routable to the LB
   (see §4) and that the satellite security group allows the LB's health-check/traffic.

This keeps LB provisioning in infrastructure-as-code (where cross-account ownership is explicit) and
lets Kubernetes manage only the dynamic target set.

## 4. `ip` target routability and CIDR rules

A target group with `target type = ip` accepts IPs from:

- the target group VPC's subnets, **or**
- `10.0.0.0/8`, `100.64.0.0/10`, `172.16.0.0/12`, `192.168.0.0/16` (RFC 1918 + RFC 6598).

**Publicly routable IPs cannot be registered.** Targets outside the target group VPC (peered VPCs,
or networks reached via TGW / Direct Connect / Site-to-Site VPN) are explicitly supported as long as
their IPs fall in those ranges and are routable to the load balancer.

For this project that means:

- Satellite pod IPs (`10.x.0.0/16`) and secondary-CIDR custom-networking IPs (`100.64.x`) are valid
  targets.
- The satellite VPC CIDR must be routable to the LB over the TGW/peering — the same connectivity we
  already require, and the satellite node CIDR is already declared in the cluster's
  `RemoteNetworkConfig`.
- The security group on the target pods' ENIs must allow the load balancer's traffic and health
  checks from the LB's subnets/SG.

## 5. Sharing one target group across sources: multi-cluster mode

By default the LBC "assumes full control over the configured target groups" and **deregisters any
target not currently in its cluster.** If you want a single target group fed by more than one source
(e.g. cluster-VPC pods *and* satellite pods registered by separate bindings, or multiple clusters),
enable **multi-cluster mode** so each controller manages only its own targets:

- Service (NLB): `service.beta.kubernetes.io/aws-load-balancer-multi-cluster-target-group: "true"`
- Ingress (ALB): `alb.ingress.kubernetes.io/multi-cluster-target-group: "true"`
- `TargetGroupBinding`: `multiClusterTargetGroup: true` in the spec

Each controller snapshots the set of targets it owns into a ConfigMap
(`aws-lbc-targets-<targetgroupbinding-name>`) and only reconciles those, so it won't deregister
targets another source added. Without this, two sources sharing a target group will fight, each
removing the other's targets.

## 6. ALB vs NLB

- **NLB** (L4) with `target-type: ip` is AWS's recommended choice for reaching pods on remote/hybrid
  nodes when traffic originates from an AWS Region — the satellite case. It forwards straight to pod
  IPs, bypassing the Service network path.
- **ALB** (L7) likewise supports `ip` targets and the same `TargetGroupBinding` + multi-cluster
  fields; use it when you need L7 routing/host/path rules.
- For either, the cross-VPC/account story is the **`TargetGroupBinding`** path in §3, not the
  auto-provision-from-annotations path.

## 7. Decision guide

| Goal | Approach |
|---|---|
| Expose a **cluster-VPC** workload | Automatic path — `Service type: LoadBalancer` / `Ingress` annotations (LBC provisions the LB) |
| Expose **satellite-node** pods (different VPC, same account) | Self-managed LB + `TargetGroupBinding` (`targetType: ip`, `vpcID` set); pod IPs routable over TGW |
| Expose **satellite-node** pods in a **different account** | As above, plus `iamRoleArnToAssume` + `assumeRoleExternalId` on the TGB |
| One target group fed by multiple sources/clusters | Enable **multi-cluster mode** on every source |
| Run a second LBC for the satellite region | Not supported — one controller deployment per cluster; use `TargetGroupBinding` instead |

## 8. Why this differs from the other AWS controllers

The pattern across satellite-facing AWS controllers:

| Controller | Acts on resources in the satellite account/region? | Satellite story |
|---|---|---|
| VPC CNI / IPAMD | yes (ENIs) | dedicated `aws-node-satellite-<acct>-<region>` DS, satellite instance role via IMDS |
| EBS CSI controller | yes (volumes) | per-`(account,region)` controller **and** a forked, renamed driver |
| Cluster Autoscaler | yes (ASG) | per-region CA deployment, cross-account via Pod Identity `--target-role-arn` |
| **AWS Load Balancer Controller** | **no, when using `TargetGroupBinding`** — it only registers targets, and assumes a role for cross-account registration | **single controller** + `TargetGroupBinding` with built-in `vpcID` / `iamRoleArnToAssume` |
| Node-local (Instance Store CSI, local provisioner) | no AWS calls | works as-is |

The LBC is the only one that ships a **purpose-built cross-account field** (`iamRoleArnToAssume`),
so cross-account load balancing is a supported configuration rather than a workaround.
