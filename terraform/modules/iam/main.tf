# =============================================================================
# Cross-Region Node IAM Role
# =============================================================================

resource "aws_iam_role" "node" {
  name = var.node_role_name

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

# Managed policy attachments
resource "aws_iam_role_policy_attachment" "worker_node" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
  role       = aws_iam_role.node.name
}

resource "aws_iam_role_policy_attachment" "ecr_read" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
  role       = aws_iam_role.node.name
}

resource "aws_iam_role_policy_attachment" "cni" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
  role       = aws_iam_role.node.name
}

resource "aws_iam_role_policy_attachment" "ssm" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
  role       = aws_iam_role.node.name
}

# Inline policy for xrn-install preflight checks
resource "aws_iam_role_policy" "xrn_preflight" {
  name = "xrn-preflight"
  role = aws_iam_role.node.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "eks:ListAccessEntries",
        "eks:DescribeAccessEntry",
        "eks:DescribeCluster"
      ]
      Resource = "*"
    }]
  })
}

# =============================================================================
# Instance Profile
# =============================================================================

resource "aws_iam_instance_profile" "node" {
  name = "${var.node_role_name}-profile"
  role = aws_iam_role.node.name
}

# =============================================================================
# EKS Access Entry (HYBRID_LINUX)
# =============================================================================

resource "aws_eks_access_entry" "hybrid_linux" {
  cluster_name  = var.cluster_name
  principal_arn = aws_iam_role.node.arn
  type          = "HYBRID_LINUX"
}
