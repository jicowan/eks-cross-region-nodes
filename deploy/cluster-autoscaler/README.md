# Cluster Autoscaler — scoped to satellite ASG only

Installs Cluster Autoscaler in the cluster, configured to manage **only** the satellite ASG `cross-region-main-asg` in `eu-west-1`. CA runs on the cluster-VPC nodes (us-east-2) and calls EC2/ASG APIs cross-region.

## Why explicit ASG list (not auto-discovery)

CA supports two modes:

- `--node-group-auto-discovery=asg:tag=k8s.io/cluster-autoscaler/main` — finds every ASG with that tag
- `--nodes=MIN:MAX:asg-name` — manages exactly the ASG(s) you list

We use **explicit list**. Auto-discovery would also pick up any ASG in the cluster region tagged for this cluster, including ASGs you might not want CA managing. Explicit list = guaranteed scope.

## Files

| File | Purpose |
|---|---|
| `iam-policy.json` | IAM permissions for the CA role (autoscaling + EC2 describe + scaling actions on `cross-region-*` ASGs) |
| `setup-iam.sh` | Creates the IAM role, attaches the policy, ensures the eks-pod-identity-agent addon is installed, and creates a Pod Identity association binding the role to `kube-system/cluster-autoscaler` |
| `cluster-autoscaler.yaml` | ServiceAccount, RBAC, and Deployment for CA |
| `test-scale.yaml` | A test Deployment that triggers CA scale-up to 2 nodes |

## Authentication: EKS Pod Identity

CA uses **EKS Pod Identity** (not IRSA). The `eks-pod-identity-agent` addon delivers credentials to the CA pod via the SDK's container credential provider — no token files, no annotations on the SA, no OIDC provider setup.

Trust policy (set by `setup-iam.sh`):

```json
{
  "Effect": "Allow",
  "Principal": {"Service": "pods.eks.amazonaws.com"},
  "Action": ["sts:AssumeRole", "sts:TagSession"]
}
```

The IAM permissions are scoped tightly:
- **Read** actions (`Describe*`) on all ASGs/EC2 (CA needs to enumerate the cluster)
- **Write** actions (`SetDesiredCapacity`, `TerminateInstanceInAutoScalingGroup`) only on ASGs tagged `k8s.io/cluster-autoscaler/main=owned`

The `cross-region-main-asg` ASG already has that tag (set by `deploy/asg/create-asg.sh`).

## Cross-region operation

CA runs in the cluster region (us-east-2) but the ASG is in eu-west-1. The `AWS_REGION=eu-west-1` env var on the CA container directs all AWS SDK calls to eu-west-1.

CA only manages **one** region per process. If you add ASGs in a third region (e.g., ap-south-1), run a second CA Deployment with `AWS_REGION=ap-south-1` and a different `--nodes=...` value

## Pod placement

CA runs on cluster-VPC nodes only — it has nodeAffinity that excludes nodes with `eks.amazonaws.com/compute-type=cross-region`. If CA were to land on a satellite node, a network blip or node failure could leave the ASG without a controller.

## Apply

```bash
# 1. Create IAM role + Pod Identity association
./setup-iam.sh

# 2. Deploy CA
kubectl apply -f cluster-autoscaler.yaml

# 3. Watch logs
kubectl logs -n kube-system -l app=cluster-autoscaler -f
```

You should see lines like:

```
I0604 ... main.go:497] Cluster Autoscaler 1.36.0
I0604 ... aws_cloud_provider.go:386] Successfully load 1 ASGs
I0604 ... static_autoscaler.go:... Starting main loop
```

## Test scale-up

Deploy the test workload that requests more capacity than the current node has:

```bash
kubectl apply -f test-scale.yaml

# Watch pods stay Pending while CA notices
kubectl get pods -l app=scale-test -w

# Watch CA log a scale-up decision
kubectl logs -n kube-system -l app=cluster-autoscaler -f | grep -i "scale up"

# Watch the new node appear
kubectl get nodes -l eks.amazonaws.com/compute-type=cross-region -w
```

CA scales the ASG to 2, the new node joins (~2 minutes), pods schedule. After cleaning up the test workload, CA will scale back to 1 (or 0 if `--scale-down-utilization-threshold` allows) after `--scale-down-unneeded-time` (default 10 minutes).

## Test scale-down

```bash
kubectl delete -f test-scale.yaml

# After ~10 minutes of idleness, CA will scale the ASG down
# Watch: kubectl logs -n kube-system -l app=cluster-autoscaler -f | grep -i "scale down"
```

## Adding more satellite ASGs

1. Create the new ASG in its region (e.g., `cross-region-main-ap-south-asg` in ap-south-1)
2. Decide: same-region CA or new CA?
   - **Same region as existing CA** — just add `--nodes=0:N:new-asg-name` to the existing CA Deployment
   - **Different region** — deploy a second CA with `AWS_REGION=<new-region>` and `--nodes=...` for that ASG only
3. Either way, ensure the IAM policy allows `SetDesiredCapacity` and `TerminateInstanceInAutoScalingGroup` on the new ASG (current policy uses tag-based conditional; works automatically if the new ASG has the same `k8s.io/cluster-autoscaler/main=owned` tag)

## Troubleshooting

**CA pod CrashLoopBackOff with "Failed to create AWS Manager: cannot autodiscover ASGs":**
- The `--nodes=` flag value doesn't match a real ASG. Check ASG name is exactly `cross-region-main-asg` and exists in eu-west-1.

**CA logs "AccessDenied" on `autoscaling:DescribeAutoScalingGroups`:**
- Pod Identity association didn't get attached. Run:
  ```bash
  aws eks list-pod-identity-associations --region us-east-2 --cluster-name main \
    --namespace kube-system --service-account cluster-autoscaler
  ```
  If empty, re-run `./setup-iam.sh`.

**CA scales up but nodes never join:**
- This is the cross-region setup itself, not CA. Check the new instance via SSM:
  ```bash
  aws ssm send-command --region eu-west-1 --instance-ids <id> \
    --document-name AWS-RunShellScript \
    --parameters 'commands=["systemctl status kubelet","journalctl -u kubelet -n 50"]'
  ```
- Verify `RemoteNetworkConfig` still includes `10.1.0.0/16`
- Manually approve the kubelet-serving CSR for the new node

**CA never scales down:**
- Check `--scale-down-unneeded-time` (default 10m) hasn't elapsed yet
- Check the node has no pods that block eviction (system pods, pods without controllers, pods with local storage). Add `--skip-nodes-with-local-storage=false` (already set) or `--skip-nodes-with-system-pods=false` (already set)
