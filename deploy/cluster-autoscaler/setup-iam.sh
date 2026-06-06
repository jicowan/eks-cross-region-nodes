#!/bin/bash
# setup-iam.sh — Create the IAM role for Cluster Autoscaler and associate it with the
# kube-system/cluster-autoscaler service account via EKS Pod Identity.
#
# Run this BEFORE applying cluster-autoscaler.yaml.
#
# Edit the configuration below for your cluster. CLUSTER_NAME is also substituted into the
# scaling policy's tag condition (k8s.io/cluster-autoscaler/<CLUSTER_NAME>=owned), so it must
# match the tag on your satellite ASG (deploy/asg/create-asg.sh sets that tag automatically).

set -euo pipefail

# ---- Configuration (edit these) ----
CLUSTER_NAME="main"
CLUSTER_REGION="us-east-2"
ROLE_NAME="ClusterAutoscalerRole"
POLICY_NAME="ClusterAutoscalerPolicy"
NAMESPACE="kube-system"
SERVICE_ACCOUNT="cluster-autoscaler"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 1. Create the IAM role with EKS Pod Identity trust policy
echo "[1/4] Creating IAM role $ROLE_NAME..."
cat > /tmp/ca-trust-policy.json <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Principal": {"Service": "pods.eks.amazonaws.com"},
    "Action": ["sts:AssumeRole", "sts:TagSession"]
  }]
}
EOF

if aws iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
  echo "  Role $ROLE_NAME already exists, skipping creation"
else
  aws iam create-role \
    --role-name "$ROLE_NAME" \
    --assume-role-policy-document file:///tmp/ca-trust-policy.json >/dev/null
fi

# 2. Attach the policy. The scaling-action statement is scoped by the tag
#    k8s.io/cluster-autoscaler/<CLUSTER_NAME>=owned, so substitute CLUSTER_NAME into the policy
#    template before applying.
echo "[2/4] Attaching policy $POLICY_NAME (scoped to k8s.io/cluster-autoscaler/${CLUSTER_NAME}=owned)..."
RENDERED_POLICY=$(mktemp)
trap "rm -f /tmp/ca-trust-policy.json $RENDERED_POLICY" EXIT
sed "s|CLUSTER_NAME_PLACEHOLDER|${CLUSTER_NAME}|g" "$SCRIPT_DIR/iam-policy.json" > "$RENDERED_POLICY"
aws iam put-role-policy \
  --role-name "$ROLE_NAME" \
  --policy-name "$POLICY_NAME" \
  --policy-document "file://$RENDERED_POLICY"

# 3. Verify the eks-pod-identity-agent addon is installed (Pod Identity requires it)
echo "[3/4] Verifying eks-pod-identity-agent addon..."
if aws eks describe-addon --region "$CLUSTER_REGION" --cluster-name "$CLUSTER_NAME" \
    --addon-name eks-pod-identity-agent >/dev/null 2>&1; then
  echo "  eks-pod-identity-agent is installed"
else
  echo "  Installing eks-pod-identity-agent addon..."
  aws eks create-addon --region "$CLUSTER_REGION" --cluster-name "$CLUSTER_NAME" \
    --addon-name eks-pod-identity-agent
  echo "  Waiting for addon to become ACTIVE..."
  aws eks wait addon-active --region "$CLUSTER_REGION" --cluster-name "$CLUSTER_NAME" \
    --addon-name eks-pod-identity-agent
fi

# 4. Associate the IAM role with the service account
echo "[4/4] Creating Pod Identity association..."
ACCOUNT_ID=$(aws sts get-caller-identity --query 'Account' --output text)
ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}"

EXISTING=$(aws eks list-pod-identity-associations --region "$CLUSTER_REGION" \
  --cluster-name "$CLUSTER_NAME" \
  --namespace "$NAMESPACE" \
  --service-account "$SERVICE_ACCOUNT" \
  --query 'associations[0].associationId' --output text 2>/dev/null || echo "None")

if [ "$EXISTING" != "None" ] && [ -n "$EXISTING" ]; then
  echo "  Pod Identity association already exists: $EXISTING"
else
  aws eks create-pod-identity-association --region "$CLUSTER_REGION" \
    --cluster-name "$CLUSTER_NAME" \
    --namespace "$NAMESPACE" \
    --service-account "$SERVICE_ACCOUNT" \
    --role-arn "$ROLE_ARN" >/dev/null
  echo "  Created"
fi

echo
echo "✓ IAM setup complete."
echo
echo "Role ARN: $ROLE_ARN"
echo
echo "Next: kubectl apply -f $SCRIPT_DIR/cluster-autoscaler.yaml"
