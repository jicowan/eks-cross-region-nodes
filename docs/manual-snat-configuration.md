# Manual SNAT Configuration Without the ConfigMap Watcher

If the Phase 2 ConfigMap watcher PR is not accepted upstream into the VPC CNI, you can still manage SNAT exclusions for cross-region nodes. This document explains the available options.

## Background

Cross-region nodes need SNAT exclusions for **all VPC CIDRs** in the cluster (both the cluster VPC and every satellite VPC). Without these exclusions, pod-to-pod traffic between VPCs has its source IP rewritten to the node's primary IP, breaking NetworkPolicy, audit logs, and source-IP-aware applications.

The VPC CNI has one built-in mechanism for this: the `AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS` environment variable on the aws-node DaemonSet.

## Option 1: Static env var (simplest, requires DaemonSet restart)

Set `AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS` on the aws-node DaemonSet with all VPC CIDRs comma-separated:

```bash
kubectl set env daemonset aws-node -n kube-system \
  AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS=10.0.0.0/16,10.1.0.0/16,10.2.0.0/16
```

**Downsides:**
- Changing the value triggers a rolling restart of all aws-node pods cluster-wide.
- Every time you add or remove a satellite region, you must update this env var and wait for the rollout.
- All nodes (cluster-VPC and satellite) share the same value. Redundant entries (e.g., VPC-A's CIDR on VPC-A nodes) are harmless but untidy.

**When to use:** 1–3 satellite regions that rarely change. The operational cost of an occasional DaemonSet restart is acceptable.

## Option 2: Helm values (GitOps-friendly, still requires rollout)

If you manage the VPC CNI via the Helm chart (`aws-vpc-cni`), set the exclusion in `values.yaml`:

```yaml
env:
  AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS: "10.0.0.0/16,10.1.0.0/16,10.2.0.0/16"
```

Or via `--set` on install/upgrade:

```bash
helm upgrade aws-vpc-cni eks/aws-vpc-cni -n kube-system \
  --set env.AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS="10.0.0.0/16\,10.1.0.0/16\,10.2.0.0/16"
```

Same behavior as Option 1 (env var on the DaemonSet) but fits into GitOps workflows.

## Option 3: Init container patch (dynamic, no DaemonSet restart)

Use an init container or sidecar that reads the `aws-node-vpc-cidrs` ConfigMap and writes an iptables rule set directly. This bypasses the VPC CNI's SNAT logic entirely.

Create a DaemonSet that runs on every node and manages the SNAT exclusion rules independently:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: snat-exclusion-manager
  namespace: kube-system
spec:
  selector:
    matchLabels:
      app: snat-exclusion-manager
  template:
    metadata:
      labels:
        app: snat-exclusion-manager
    spec:
      hostNetwork: true
      tolerations:
      - operator: Exists
      containers:
      - name: manager
        image: public.ecr.aws/amazonlinux/amazonlinux:2023
        securityContext:
          privileged: true
        command:
        - /bin/bash
        - -c
        - |
          # Read CIDRs from mounted ConfigMap and apply iptables rules
          while true; do
            if [ -f /config/exclude-snat-cidrs ]; then
              while IFS= read -r cidr; do
                cidr=$(echo "$cidr" | tr -d '[:space:]')
                [ -z "$cidr" ] && continue
                # Add RETURN rule to AWS-SNAT-CHAIN-0 if not already present
                if ! iptables -t nat -C AWS-SNAT-CHAIN-0 -d "$cidr" -m comment \
                    --comment "AWS SNAT CHAIN EXCLUSION (dynamic)" -j RETURN 2>/dev/null; then
                  iptables -t nat -I AWS-SNAT-CHAIN-0 1 -d "$cidr" -m comment \
                    --comment "AWS SNAT CHAIN EXCLUSION (dynamic)" -j RETURN
                  echo "Added SNAT exclusion for $cidr"
                fi
              done < /config/exclude-snat-cidrs
            fi
            sleep 30
          done
        volumeMounts:
        - name: snat-config
          mountPath: /config
          readOnly: true
      volumes:
      - name: snat-config
        configMap:
          name: aws-node-vpc-cidrs
          optional: true
```

**How it works:**
- Mounts the `aws-node-vpc-cidrs` ConfigMap as a volume.
- Every 30 seconds, reads the CIDRs and ensures matching iptables RETURN rules exist in `AWS-SNAT-CHAIN-0`.
- Rules are inserted at position 1 (top of chain, before the SNAT rule).
- Idempotent: checks if the rule exists before adding.
- ConfigMap updates are reflected by kubelet's ConfigMap volume refresh (typically within 60s).
- No aws-node restart required.

**Downsides:**
- Runs a separate privileged DaemonSet (additional resource usage, security surface).
- Races with aws-node's own iptables management: if aws-node rebuilds its chains (e.g., on VPC CIDR change from IMDS), it may remove your dynamically-added rules. The 30-second loop re-adds them, but there's a brief window.
- Doesn't deduplicate with the env-var-based exclusions (harmless but results in duplicate iptables rules).

**When to use:** You need dynamic updates without DaemonSet restarts, and the upstream PR isn't available.

## Option 4: Fork the VPC CNI (most control, highest maintenance)

Apply the Phase 2 changes from this project to a fork of `amazon-vpc-cni-k8s` and deploy your own image.

The changes are small (3 files modified, 1 file added):

| File | Change |
|---|---|
| `pkg/ipamd/snat_configmap.go` | New — reads ConfigMap, parses CIDRs |
| `pkg/k8sapi/k8sutils.go` | Add ConfigMap to cache filter |
| `pkg/networkutils/network.go` | Add `SetExcludeSNATCIDRs`, mutex-protect the field |
| `pkg/ipamd/ipamd.go` | Merge ConfigMap CIDRs in reconcile loop |

Build and push your own image:

```bash
# Clone your fork
git clone https://github.com/<your-org>/amazon-vpc-cni-k8s
cd amazon-vpc-cni-k8s

# Apply Phase 2 changes (already in the repo if you forked from a branch with them)
# Build
make docker

# Push to your ECR
docker tag amazon/amazon-k8s-cni:latest <account>.dkr.ecr.<region>.amazonaws.com/amazon-k8s-cni:custom
docker push <account>.dkr.ecr.<region>.amazonaws.com/amazon-k8s-cni:custom

# Update the DaemonSet image
kubectl set image daemonset aws-node -n kube-system \
  aws-node=<account>.dkr.ecr.<region>.amazonaws.com/amazon-k8s-cni:custom
```

Enable the feature:

```bash
kubectl set env daemonset aws-node -n kube-system \
  AWS_VPC_K8S_CNI_ENABLE_DYNAMIC_SNAT_CFG=true
```

**Downsides:**
- You must rebase your fork on every upstream release to pick up security patches and bug fixes.
- You own the build, test, and release pipeline.
- EKS managed addon updates will overwrite your custom image unless you disable addon management.

**When to use:** You need the full ConfigMap watcher behavior and are comfortable maintaining a fork. Best for organizations with existing CNI forks (some do this for other customizations).

## Recommendation

| Scenario | Recommended option |
|---|---|
| 1–3 satellites, stable topology | Option 1 (static env var) |
| GitOps-managed clusters | Option 2 (Helm values) |
| Frequent satellite additions, can't restart aws-node | Option 3 (sidecar DaemonSet) |
| Full control, have CI/CD for container images | Option 4 (fork) |

For most users doing the Phase 1 proof of concept, **Option 1 is sufficient**. The DaemonSet restart takes ~60 seconds on a typical cluster and doesn't interrupt running pods — it only briefly delays new pod scheduling while aws-node restarts on each node.

## Interaction with `xrnctl`

`xrnctl add-region` always writes the `aws-node-vpc-cidrs` ConfigMap regardless of which SNAT option you choose. If you're using Option 1 or 2, ignore the ConfigMap and manage the env var manually (or have `xrnctl` also print the command to update it). If you're using Option 3, the ConfigMap is consumed automatically by the sidecar DaemonSet. If you're using Option 4, the ConfigMap is consumed by the patched VPC CNI.
