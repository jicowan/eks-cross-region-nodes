# =============================================================================
# Node Group IAM Role
# =============================================================================

resource "aws_iam_role" "node_group" {
  name = "${var.cluster_name}-local-node-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
    }]
  })
}

resource "aws_iam_role_policy_attachment" "worker_node" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
  role       = aws_iam_role.node_group.name
}

resource "aws_iam_role_policy_attachment" "ecr_read" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
  role       = aws_iam_role.node_group.name
}

resource "aws_iam_role_policy_attachment" "cni" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
  role       = aws_iam_role.node_group.name
}

# =============================================================================
# Managed Node Group
# =============================================================================

resource "aws_eks_node_group" "local" {
  cluster_name    = var.cluster_name
  node_group_name = "${var.cluster_name}-local-nodes"
  node_role_arn   = aws_iam_role.node_group.arn
  subnet_ids      = var.subnet_ids
  version         = var.cluster_version

  instance_types = [var.instance_type]

  scaling_config {
    desired_size = var.desired_count
    max_size     = var.desired_count + 1
    min_size     = 1
  }

  update_config {
    max_unavailable = 1
  }

  labels = {
    "node-role" = "local"
  }

  tags = {
    Name = "${var.cluster_name}-local-node"
  }

  depends_on = [
    aws_iam_role_policy_attachment.worker_node,
    aws_iam_role_policy_attachment.ecr_read,
    aws_iam_role_policy_attachment.cni,
  ]
}
