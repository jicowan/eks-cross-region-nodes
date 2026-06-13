#!/bin/bash
# create-asg.sh — Create a launch template and ASG in a satellite region for cross-region EKS nodes.
#
# This is the SAME-ACCOUNT / cross-region path (satellite VPC in the cluster's own account, another
# region). For a CROSS-ACCOUNT satellite, use the cross-account user-data template instead — see
# README-cross-account.md.
#
# Prerequisites (must already exist):
#   - Node IAM role + instance profile + HYBRID_LINUX access entry — create with:
#       xrnctl setup-iam --cluster-name <name> --cluster-region <region> --node-role-name CrossRegionNodeRole
#   - Satellite registered (ConfigMap SNAT CIDRs + RemoteNetworkConfig) — create with:
#       xrnctl add-satellite --cluster-name <name> --cluster-region <region> --vpc-id <vpc> --satellite-region <region>
#   - Cluster SG allows TCP 443 (+ 53/pods/10250) from the satellite VPC CIDR
#   - TGW peering / routes between cluster and satellite VPCs
#   - xrn-install released to https://github.com/jicowan/eks-cross-region-nodes/releases
#
# Usage:
#   ./create-asg.sh

set -euo pipefail

# ---- Configuration (edit these — placeholders below are EXAMPLES, replace with your values) ----
CLUSTER_NAME="main"
CLUSTER_REGION="us-east-2"
SATELLITE_REGION="eu-west-1"
SATELLITE_VPC_ID="vpc-xxxxxxxxxxxxxxxxx"
SATELLITE_SUBNET_IDS="subnet-xxxxxxxxxxxxxxxxx,subnet-yyyyyyyyyyyyyyyyy"  # comma-separated
SATELLITE_NODE_SG="sg-xxxxxxxxxxxxxxxxx"
INSTANCE_PROFILE_NAME="CrossRegionNodeProfile"
INSTANCE_TYPE="m6i.xlarge"
# URL the node downloads xrn-install from. /latest/download/ redirects to the newest release.
XRN_INSTALL_URL="https://github.com/jicowan/eks-cross-region-nodes/releases/latest/download/xrn-install-linux-amd64"

LAUNCH_TEMPLATE_NAME="cross-region-${CLUSTER_NAME}-lt"
ASG_NAME="cross-region-${CLUSTER_NAME}-asg"
ASG_MIN_SIZE=0
ASG_MAX_SIZE=3
ASG_DESIRED_CAPACITY=1

# ---- Discover cluster details ----
echo "[1/5] Describing cluster ${CLUSTER_NAME} in ${CLUSTER_REGION}..."
DESCRIBE=$(aws eks describe-cluster --region "$CLUSTER_REGION" --name "$CLUSTER_NAME")
ENDPOINT=$(echo "$DESCRIBE" | jq -r '.cluster.endpoint')
CA=$(echo "$DESCRIBE" | jq -r '.cluster.certificateAuthority.data')
SERVICE_CIDR=$(echo "$DESCRIBE" | jq -r '.cluster.kubernetesNetworkConfig.serviceIpv4Cidr')

if [[ -z "$ENDPOINT" || -z "$CA" || -z "$SERVICE_CIDR" ]]; then
  echo "Error: could not extract cluster details from describe-cluster output" >&2
  exit 1
fi
echo "  Endpoint: $ENDPOINT"
echo "  Service CIDR: $SERVICE_CIDR"

# ---- Look up the latest AL2023 standard EKS-optimized AMI in the satellite region ----
echo "[2/5] Looking up latest AL2023 EKS-optimized AMI for satellite region ${SATELLITE_REGION}..."
K8S_VERSION=$(echo "$DESCRIBE" | jq -r '.cluster.version')
AMI_PARAM="/aws/service/eks/optimized-ami/${K8S_VERSION}/amazon-linux-2023/x86_64/standard/recommended/image_id"
AMI_ID=$(aws ssm get-parameter --region "$SATELLITE_REGION" --name "$AMI_PARAM" --query 'Parameter.Value' --output text)
echo "  AMI: $AMI_ID (k8s ${K8S_VERSION})"

# ---- Render user-data ----
echo "[3/5] Rendering user-data template..."
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
USERDATA_FILE=$(mktemp)
trap "rm -f $USERDATA_FILE" EXIT

# envsubst-style substitution without depending on envsubst
sed \
  -e "s|\${CLUSTER_NAME}|${CLUSTER_NAME}|g" \
  -e "s|\${CLUSTER_REGION}|${CLUSTER_REGION}|g" \
  -e "s|\${CLUSTER_ENDPOINT}|${ENDPOINT}|g" \
  -e "s|\${CLUSTER_CA}|${CA}|g" \
  -e "s|\${CLUSTER_SERVICE_CIDR}|${SERVICE_CIDR}|g" \
  -e "s|\${XRN_INSTALL_URL}|${XRN_INSTALL_URL}|g" \
  "${SCRIPT_DIR}/userdata.template.txt" > "$USERDATA_FILE"

# user-data must be base64 for AWS CLI launch template input
USERDATA_B64=$(base64 < "$USERDATA_FILE" | tr -d '\n')

# ---- Create or update the launch template ----
echo "[4/5] Creating launch template ${LAUNCH_TEMPLATE_NAME}..."

# Build the LaunchTemplateData JSON
LT_DATA=$(jq -nc \
  --arg ami "$AMI_ID" \
  --arg type "$INSTANCE_TYPE" \
  --arg profile "$INSTANCE_PROFILE_NAME" \
  --arg sg "$SATELLITE_NODE_SG" \
  --arg userdata "$USERDATA_B64" \
  --arg cluster "$CLUSTER_NAME" \
  '{
    ImageId: $ami,
    InstanceType: $type,
    IamInstanceProfile: {Name: $profile},
    NetworkInterfaces: [{DeviceIndex: 0, Groups: [$sg], AssociatePublicIpAddress: false}],
    UserData: $userdata,
    MetadataOptions: {HttpTokens: "required", HttpPutResponseHopLimit: 2, HttpEndpoint: "enabled"},
    TagSpecifications: [
      {ResourceType: "instance", Tags: [
        {Key: "Name", Value: ("cross-region-" + $cluster)},
        {Key: "eks-cross-region-poc", Value: "true"}
      ]}
    ]
  }')

# Check if launch template already exists
if aws ec2 describe-launch-templates --region "$SATELLITE_REGION" \
    --launch-template-names "$LAUNCH_TEMPLATE_NAME" >/dev/null 2>&1; then
  echo "  Launch template exists; creating new version..."
  LT_VERSION=$(aws ec2 create-launch-template-version --region "$SATELLITE_REGION" \
    --launch-template-name "$LAUNCH_TEMPLATE_NAME" \
    --launch-template-data "$LT_DATA" \
    --query 'LaunchTemplateVersion.VersionNumber' --output text)
  aws ec2 modify-launch-template --region "$SATELLITE_REGION" \
    --launch-template-name "$LAUNCH_TEMPLATE_NAME" \
    --default-version "$LT_VERSION" >/dev/null
  echo "  New default version: $LT_VERSION"
else
  aws ec2 create-launch-template --region "$SATELLITE_REGION" \
    --launch-template-name "$LAUNCH_TEMPLATE_NAME" \
    --launch-template-data "$LT_DATA" \
    --query 'LaunchTemplate.LaunchTemplateId' --output text
fi

# ---- Create or update the ASG ----
echo "[5/5] Creating Auto Scaling group ${ASG_NAME}..."

# ASG tags must use ResourceId/ResourceType/PropagateAtLaunch flags
ASG_TAGS=$(jq -nc --arg asg "$ASG_NAME" --arg cluster "$CLUSTER_NAME" \
  '[
    {Key: "Name", Value: ("cross-region-" + $cluster), PropagateAtLaunch: true},
    {Key: "eks-cross-region-poc", Value: "true", PropagateAtLaunch: true},
    # Cluster Autoscaler discovery tags
    {Key: ("k8s.io/cluster-autoscaler/" + $cluster), Value: "owned", PropagateAtLaunch: true},
    {Key: "k8s.io/cluster-autoscaler/enabled", Value: "true", PropagateAtLaunch: true},
    # Topology hints for CA so it can pick the right ASG for a region/zone
    {Key: "k8s.io/cluster-autoscaler/node-template/label/eks.amazonaws.com/compute-type", Value: "cross-region", PropagateAtLaunch: false},
    {Key: "k8s.io/cluster-autoscaler/node-template/label/topology.kubernetes.io/region", Value: "'"$SATELLITE_REGION"'", PropagateAtLaunch: false}
  ]')
ASG_TAGS_FILE=$(mktemp)
trap "rm -f $USERDATA_FILE $ASG_TAGS_FILE" EXIT
echo "$ASG_TAGS" > "$ASG_TAGS_FILE"

if aws autoscaling describe-auto-scaling-groups --region "$SATELLITE_REGION" \
    --auto-scaling-group-names "$ASG_NAME" \
    --query 'AutoScalingGroups[0].AutoScalingGroupName' --output text 2>/dev/null | grep -q "$ASG_NAME"; then
  echo "  ASG exists; updating launch template version..."
  aws autoscaling update-auto-scaling-group --region "$SATELLITE_REGION" \
    --auto-scaling-group-name "$ASG_NAME" \
    --launch-template "LaunchTemplateName=$LAUNCH_TEMPLATE_NAME,Version=\$Default" \
    --min-size "$ASG_MIN_SIZE" --max-size "$ASG_MAX_SIZE" --desired-capacity "$ASG_DESIRED_CAPACITY"
else
  aws autoscaling create-auto-scaling-group --region "$SATELLITE_REGION" \
    --auto-scaling-group-name "$ASG_NAME" \
    --launch-template "LaunchTemplateName=$LAUNCH_TEMPLATE_NAME,Version=\$Default" \
    --min-size "$ASG_MIN_SIZE" --max-size "$ASG_MAX_SIZE" --desired-capacity "$ASG_DESIRED_CAPACITY" \
    --vpc-zone-identifier "$SATELLITE_SUBNET_IDS" \
    --tags "file://$ASG_TAGS_FILE"
fi

echo
echo "✓ Done."
echo
echo "Watch instances launch:"
echo "  aws ec2 describe-instances --region $SATELLITE_REGION \\"
echo "    --filters Name=tag:aws:autoscaling:groupName,Values=$ASG_NAME \\"
echo "    --query 'Reservations[].Instances[].{Id:InstanceId,State:State.Name,IP:PrivateIpAddress}'"
echo
echo "Watch nodes join (from a machine with kubectl access to the cluster):"
echo "  kubectl get nodes -l eks.amazonaws.com/compute-type=cross-region -w"
echo
echo "Reminder: kubelet-serving CSRs must be approved manually unless you have an auto-approver installed."
echo "  kubectl get csr"
