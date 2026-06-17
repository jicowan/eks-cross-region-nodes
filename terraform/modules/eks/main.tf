# =============================================================================
# EKS Cluster Module
#
# Creates a standard EKS cluster (NOT Auto Mode) with authentication_mode=API
# and security group rules allowing cross-region satellite node communication.
#
# RemoteNetworkConfig is managed by xrnctl, not Terraform.
# =============================================================================

# =============================================================================
# EKS Cluster IAM Role
# =============================================================================

resource "aws_iam_role" "cluster" {
  name = "${var.cluster_name}-cluster-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = {
        Service = "eks.amazonaws.com"
      }
    }]
  })
}

resource "aws_iam_role_policy_attachment" "cluster_policy" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
  role       = aws_iam_role.cluster.name
}

# =============================================================================
# Cluster Security Group
# =============================================================================

resource "aws_security_group" "cluster" {
  name_prefix = "${var.cluster_name}-cluster-sg-"
  description = "EKS cluster security group with cross-region inbound rules"
  vpc_id      = var.vpc_id

  tags = {
    Name = "${var.cluster_name}-cluster-sg"
  }

  lifecycle {
    create_before_destroy = true
  }
}

# Allow all egress
resource "aws_vpc_security_group_egress_rule" "cluster_all_egress" {
  security_group_id = aws_security_group.cluster.id
  description       = "Allow all outbound"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

# Inbound from satellite: API server (TCP 443)
resource "aws_vpc_security_group_ingress_rule" "satellite_api" {
  security_group_id = aws_security_group.cluster.id
  description       = "Satellite nodes - API server"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = var.satellite_vpc_cidr
}

# Inbound from satellite: CoreDNS TCP
resource "aws_vpc_security_group_ingress_rule" "satellite_dns_tcp" {
  security_group_id = aws_security_group.cluster.id
  description       = "Satellite nodes - CoreDNS TCP"
  ip_protocol       = "tcp"
  from_port         = 53
  to_port           = 53
  cidr_ipv4         = var.satellite_vpc_cidr
}

# Inbound from satellite: CoreDNS UDP
resource "aws_vpc_security_group_ingress_rule" "satellite_dns_udp" {
  security_group_id = aws_security_group.cluster.id
  description       = "Satellite nodes - CoreDNS UDP"
  ip_protocol       = "udp"
  from_port         = 53
  to_port           = 53
  cidr_ipv4         = var.satellite_vpc_cidr
}

# Inbound from satellite: kubelet
resource "aws_vpc_security_group_ingress_rule" "satellite_kubelet" {
  security_group_id = aws_security_group.cluster.id
  description       = "Satellite nodes - kubelet"
  ip_protocol       = "tcp"
  from_port         = 10250
  to_port           = 10250
  cidr_ipv4         = var.satellite_vpc_cidr
}

# Inbound from satellite: pod-to-pod / ephemeral ports
resource "aws_vpc_security_group_ingress_rule" "satellite_pods" {
  security_group_id = aws_security_group.cluster.id
  description       = "Satellite nodes - pod traffic"
  ip_protocol       = "tcp"
  from_port         = 1024
  to_port           = 65535
  cidr_ipv4         = var.satellite_vpc_cidr
}

# Self-referencing rule for cluster-internal communication
resource "aws_vpc_security_group_ingress_rule" "cluster_self" {
  security_group_id            = aws_security_group.cluster.id
  description                  = "Cluster internal"
  ip_protocol                  = "-1"
  referenced_security_group_id = aws_security_group.cluster.id
}

# =============================================================================
# EKS Cluster (Standard mode, NOT Auto Mode)
# =============================================================================

resource "aws_eks_cluster" "this" {
  name     = var.cluster_name
  version  = var.kubernetes_version
  role_arn = aws_iam_role.cluster.arn

  access_config {
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = true
  }

  vpc_config {
    subnet_ids              = var.subnet_ids
    security_group_ids      = [aws_security_group.cluster.id]
    endpoint_private_access = true
    endpoint_public_access  = true
  }

  depends_on = [
    aws_iam_role_policy_attachment.cluster_policy
  ]

  # IMPORTANT: RemoteNetworkConfig is managed by xrnctl (not Terraform).
  # Without this ignore, any terraform apply will reset remoteNetworkConfig to null,
  # breaking kubectl exec/logs to satellite nodes.
  lifecycle {
    ignore_changes = [vpc_config]
  }
}
