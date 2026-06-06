# EKS Pod Identity on cross-account satellite nodes

This explains how EKS Pod Identity behaves for pods running on a **cross-account** satellite node
(a worker node in a different AWS account from the EKS cluster), why the VPC CNI on those nodes
does *not* use Pod Identity, and how to give an ordinary application pod AWS credentials in the
satellite account.

If you only run same-account / cross-region satellites, none of this applies — Pod Identity works
exactly as it does for any in-account node.

## TL;DR

| Question | Answer |
|---|---|
| Does the Pod Identity Agent run on a cross-account node? | **Yes** — it's a DaemonSet on every node that joined the cluster. |
| Will a plain Pod Identity association give a pod *satellite-account* credentials? | **No.** It returns **cluster-account** credentials (the association's role lives in the cluster account). On a satellite node that usually means the wrong account. |
| Can Pod Identity give a pod *cross-account* (satellite-account) credentials on purpose? | **Yes** — via a cross-account association with `--target-role-arn`. The agent does the AssumeRole into the other account server-side. |
| How does the VPC CNI get satellite-account credentials, then? | **Not** via Pod Identity. Its DaemonSet (`aws-node-satellite-<acct>-<region>`) uses a ServiceAccount with **no** association, so the SDK falls through to IMDS and uses the satellite-account instance role. |

## 1. Background: how Pod Identity normally works

EKS Pod Identity associates a Kubernetes **ServiceAccount** with an **IAM role**:

```
association: (namespace, serviceAccount)  →  roleArn (in the cluster account)
```

The **EKS Pod Identity Agent** runs as a DaemonSet on every node. When a pod whose ServiceAccount
has an association makes an AWS SDK call, the SDK fetches credentials from the agent (via the
`AWS_CONTAINER_CREDENTIALS_FULL_URI` env var EKS injects into the pod) and the agent returns
credentials for the associated role. The pod never sees long-lived keys; the agent brokers
short-lived credentials.

Key property: **the role in the association is resolved in the cluster's account.** Pod Identity is
a cluster-account mechanism.

## 2. What happens on a cross-account node

The Pod Identity Agent is deployed cluster-wide, so it **is** running on a cross-account satellite
node, and it **will** answer credential requests. The catch is *which* credentials:

- A pod on the satellite node, using a ServiceAccount that has an association, gets credentials for
  the association's role — **which is in the cluster account**.
- If that pod then calls an AWS API expecting to act in the **satellite** account, the call fails.
  The token is issued in the cluster account's context; the satellite-account API rejects it
  (you'll see `InvalidTokenException` or access-denied-style errors).

This is not a bug — it's the mechanism working as designed. Pod Identity hands out cluster-account
identity. On a node whose workloads need satellite-account identity, that's the wrong account.

### Why the VPC CNI hits this

This is exactly why cross-account nodes run a **dedicated** `aws-node-satellite-<acct>-<region>`
DaemonSet instead of the stock `aws-node`:

- The stock `aws-node` ServiceAccount has a Pod Identity association (created by the `vpc-cni`
  add-on) pointing at a cluster-account role.
- On a satellite node, the agent would hand the CNI that cluster-account role, and its
  `ec2:DescribeNetworkInterfaces` / ENI-management calls against the satellite-account EC2 API would
  be rejected.
- The fix is a ServiceAccount with **no** association. With nothing to intercept, the AWS SDK falls
  through its normal credential chain to **IMDS** and uses the node's **instance role**, which is in
  the satellite account and carries the CNI permissions. No Pod Identity involved.

You can't simply put a cross-account `--target-role-arn` (next section) on the stock `aws-node`
association to fix this, because associations are per-ServiceAccount, not per-node: the stock
`aws-node` SA is shared by cluster-VPC and satellite nodes, so retargeting it would break the
in-region nodes. A separate ServiceAccount is required either way — and once you have one, IMDS is
simpler than a cross-account association for the CNI's purposes.

## 3. Giving an application pod satellite-account credentials

For your *own* pods that need to call AWS APIs in the satellite account, use Pod Identity's native
**cross-account role chaining**. An association has a source role (cluster account) and a target
role (satellite account); the agent assumes the target role server-side and the pod transparently
receives **satellite-account** credentials.

```
                cluster account (A)                         satellite account (B)
  ┌──────────────────────────────────┐        ┌────────────────────────────────────┐
  │ source role  (pods.eks...)        │        │ target role                         │
  │   may sts:AssumeRole + TagSession │──────▶│   trusts A:role/<source> with        │
  │   on B:role/<target>              │        │   sts:ExternalId condition           │
  └──────────────────────────────────┘        │   has the satellite-account perms    │
                                               └────────────────────────────────────┘
  association: (ns, sa) → source role,  --target-role-arn = B:role/<target>
```

Create the association with both roles:

```bash
aws eks create-pod-identity-association --region <cluster-region> \
  --cluster-name <cluster> \
  --namespace <ns> \
  --service-account <sa> \
  --role-arn        arn:aws:iam::<A>:role/<source-role> \
  --target-role-arn arn:aws:iam::<B>:role/<target-role>
```

Requirements:

- The **source role** (account A) trusts `pods.eks.amazonaws.com` and is allowed to `sts:AssumeRole`
  (and usually `sts:TagSession`) on the target role.
- The **target role** (account B) trusts the source role, gated by an `sts:ExternalId` condition.
  EKS auto-generates the external id in the form
  `<region>/<account-A>/<cluster-name>/<namespace>/<service-account>`; the target role's trust
  policy must allow it. This is confused-deputy protection — keep it.
- The target role carries whatever satellite-account permissions the workload needs.
- **One target role per ServiceAccount.** For N satellite accounts you need N ServiceAccounts /
  associations (e.g. one workload Deployment per account).

The pod itself needs no AWS config beyond the usual SDK defaults (and `AWS_REGION` pointed at the
satellite region if it's calling regional services there). The chaining is invisible to the pod.

This is the same mechanism the cross-account **Cluster Autoscaler** uses: CA runs in the cluster but
manages an ASG in the satellite account through a `--target-role-arn` association.

## 4. Decision guide

| Your pod needs… | Use |
|---|---|
| AWS credentials in the **cluster** account | a normal Pod Identity association (`--role-arn` only) — works on any node, including satellite nodes |
| AWS credentials in the **satellite** account | a cross-account association (`--role-arn` + `--target-role-arn`) under a dedicated ServiceAccount |
| To be the VPC CNI on a cross-account node | nothing — that's handled by the `aws-node-satellite-<acct>-<region>` DaemonSet, which uses the node's IMDS instance role, not Pod Identity |

## 5. Gotchas

- **"Pod Identity is broken on my satellite node."** Usually it isn't — it's returning cluster-account
  credentials and your call needs satellite-account ones. Check whether the association has a
  `--target-role-arn`; without one, the pod gets cluster-account identity.
- **Don't retarget a shared ServiceAccount.** Putting a `--target-role-arn` on a ServiceAccount used
  by both cluster-VPC and satellite pods sends *all* of them to the satellite account. Use a
  dedicated ServiceAccount scoped (via nodeAffinity/scheduling) to the satellite workload.
- **External id mismatch.** If the target role's trust policy doesn't allow the auto-generated
  external id (`<region>/<account>/<cluster>/<ns>/<sa>`), the AssumeRole fails. Recreate the trust
  policy to match the exact namespace/ServiceAccount you associated.
- **The agent must be present.** Cross-account credentials still flow through the Pod Identity Agent
  DaemonSet on the satellite node. If the node didn't fully join (no CNI, NotReady), the agent won't
  be running and nothing gets credentials.
