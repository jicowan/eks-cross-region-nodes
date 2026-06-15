terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}

# =============================================================================
# Satellite ASG Module
#
# Creates a launch template + Auto Scaling group in the satellite region.
# Nodes bootstrap via cloud-boothook + xrn-install, register with the cluster
# as HYBRID_LINUX nodes labeled compute-type=cross-region.
# ASG is tagged for Cluster Autoscaler discovery.
# =============================================================================

# =============================================================================
# AMI Lookup — Latest AL2023 EKS-optimized AMI
# =============================================================================

data "aws_ssm_parameter" "eks_ami" {
  name = "/aws/service/eks/optimized-ami/${var.kubernetes_version}/amazon-linux-2023/x86_64/standard/recommended/image_id"
}

# =============================================================================
# Satellite Security Group
# =============================================================================

resource "aws_security_group" "satellite_node" {
  name_prefix = "${var.cluster_name}-satellite-node-"
  description = "Security group for satellite cross-region nodes"
  vpc_id      = var.vpc_id

  tags = {
    Name = "${var.cluster_name}-satellite-node-sg"
  }

  lifecycle {
    create_before_destroy = true
  }
}

# Allow all egress
resource "aws_vpc_security_group_egress_rule" "satellite_all_egress" {
  security_group_id = aws_security_group.satellite_node.id
  description       = "Allow all outbound"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"
}

# Inbound from cluster VPC: pod traffic
resource "aws_vpc_security_group_ingress_rule" "cluster_pods" {
  security_group_id = aws_security_group.satellite_node.id
  description       = "Cluster VPC - pod traffic"
  ip_protocol       = "tcp"
  from_port         = 1024
  to_port           = 65535
  cidr_ipv4         = var.cluster_vpc_cidr
}

# Inbound from cluster VPC: kubelet
resource "aws_vpc_security_group_ingress_rule" "cluster_kubelet" {
  security_group_id = aws_security_group.satellite_node.id
  description       = "Cluster VPC - kubelet"
  ip_protocol       = "tcp"
  from_port         = 10250
  to_port           = 10250
  cidr_ipv4         = var.cluster_vpc_cidr
}

# Self-referencing for intra-satellite node communication
resource "aws_vpc_security_group_ingress_rule" "satellite_self" {
  security_group_id            = aws_security_group.satellite_node.id
  description                  = "Satellite node internal"
  ip_protocol                  = "-1"
  referenced_security_group_id = aws_security_group.satellite_node.id
}

# =============================================================================
# Launch Template
# =============================================================================

resource "aws_launch_template" "satellite" {
  name_prefix = "${var.cluster_name}-satellite-"
  image_id    = data.aws_ssm_parameter.eks_ami.value

  instance_type = var.instance_type

  iam_instance_profile {
    name = var.instance_profile_name
  }

  network_interfaces {
    associate_public_ip_address = false
    security_groups             = [aws_security_group.satellite_node.id]
  }

  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 2
    http_endpoint               = "enabled"
  }

  user_data = base64encode(templatefile("${path.module}/userdata.tftpl", {
    cluster_name         = var.cluster_name
    cluster_region       = var.cluster_region
    cluster_endpoint     = var.cluster_endpoint
    cluster_ca           = var.cluster_ca
    cluster_service_cidr = var.cluster_service_cidr
    xrn_install_url      = var.xrn_install_url
  }))

  tag_specifications {
    resource_type = "instance"
    tags = {
      Name                     = "${var.cluster_name}-satellite-node"
      "eks-cross-region-poc"   = "true"
    }
  }

  lifecycle {
    create_before_destroy = true
  }
}

# =============================================================================
# Auto Scaling Group
# =============================================================================

resource "aws_autoscaling_group" "satellite" {
  name                = "${var.cluster_name}-satellite-asg"
  vpc_zone_identifier = var.subnet_ids
  min_size            = 0
  max_size            = var.max_size
  desired_capacity    = var.desired_capacity

  launch_template {
    id      = aws_launch_template.satellite.id
    version = "$Latest"
  }

  tag {
    key                 = "Name"
    value               = "${var.cluster_name}-satellite-node"
    propagate_at_launch = true
  }

  tag {
    key                 = "eks-cross-region-poc"
    value               = "true"
    propagate_at_launch = true
  }

  # Cluster Autoscaler discovery tags
  tag {
    key                 = "k8s.io/cluster-autoscaler/${var.cluster_name}"
    value               = "owned"
    propagate_at_launch = true
  }

  tag {
    key                 = "k8s.io/cluster-autoscaler/enabled"
    value               = "true"
    propagate_at_launch = true
  }

  tag {
    key                 = "k8s.io/cluster-autoscaler/node-template/label/eks.amazonaws.com/compute-type"
    value               = "cross-region"
    propagate_at_launch = false
  }

  tag {
    key                 = "k8s.io/cluster-autoscaler/node-template/label/topology.kubernetes.io/region"
    value               = var.satellite_region
    propagate_at_launch = false
  }
}
