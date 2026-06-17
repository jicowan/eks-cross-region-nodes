# =============================================================================
# EKS Cross-Region Nodes — Root Module
#
# Provisions a complete cross-region EKS environment: cluster VPC + EKS cluster
# in the cluster region, satellite VPC + ASG in the satellite region, connected
# via Transit Gateway peering.
# =============================================================================

locals {
  cluster_name = var.cluster_name
}

# =============================================================================
# VPCs
# =============================================================================

module "cluster_vpc" {
  source = "./modules/vpc"

  name_prefix = "${local.cluster_name}-cluster"
  vpc_cidr    = var.cluster_vpc_cidr
  az_count    = 2
}

module "satellite_vpc" {
  source = "./modules/vpc"

  providers = {
    aws = aws.satellite
  }

  name_prefix = "${local.cluster_name}-satellite"
  vpc_cidr    = var.satellite_vpc_cidr
  az_count    = 2
}

# =============================================================================
# EKS Cluster (standard mode, not Auto Mode)
# =============================================================================

module "eks" {
  source = "./modules/eks"

  cluster_name       = local.cluster_name
  kubernetes_version = var.kubernetes_version
  subnet_ids         = module.cluster_vpc.private_subnet_ids
  vpc_id             = module.cluster_vpc.vpc_id
  satellite_vpc_cidr = var.satellite_vpc_cidr
}

# =============================================================================
# Local Managed Node Group (CoreDNS, kube-proxy, cluster-autoscaler, CSR approver)
# =============================================================================

module "local_nodes" {
  source = "./modules/local-nodes"

  cluster_name    = module.eks.cluster_name
  cluster_version = module.eks.cluster_version
  subnet_ids      = module.cluster_vpc.private_subnet_ids
  instance_type   = var.local_node_instance_type
  desired_count   = var.local_node_count
}

# =============================================================================
# Transit Gateway (cross-region peering)
# =============================================================================

module "transit_gateway" {
  source = "./modules/transit-gateway"

  providers = {
    aws           = aws
    aws.satellite = aws.satellite
  }

  cluster_name              = local.cluster_name
  cluster_vpc_id            = module.cluster_vpc.vpc_id
  cluster_vpc_cidr          = var.cluster_vpc_cidr
  cluster_private_subnet_ids = module.cluster_vpc.private_subnet_ids
  cluster_route_table_ids   = module.cluster_vpc.private_route_table_ids

  satellite_vpc_id            = module.satellite_vpc.vpc_id
  satellite_vpc_cidr          = var.satellite_vpc_cidr
  satellite_private_subnet_ids = module.satellite_vpc.private_subnet_ids
  satellite_route_table_ids   = module.satellite_vpc.private_route_table_ids

  cluster_region   = var.cluster_region
  satellite_region = var.satellite_region
}

# =============================================================================
# IAM (cross-region node role + instance profile + access entry)
# =============================================================================

module "iam" {
  source = "./modules/iam"

  cluster_name = module.eks.cluster_name
  cluster_arn  = module.eks.cluster_arn
  node_role_name = "CrossRegionNodeRole-${local.cluster_name}"
}

# =============================================================================
# Satellite ASG (launch template + auto scaling group in satellite region)
# =============================================================================

module "satellite_asg" {
  source = "./modules/satellite-asg"

  providers = {
    aws = aws.satellite
  }

  cluster_name         = module.eks.cluster_name
  cluster_region       = var.cluster_region
  cluster_endpoint     = module.eks.cluster_endpoint
  cluster_ca           = module.eks.cluster_ca
  cluster_service_cidr = module.eks.cluster_service_cidr
  cluster_vpc_cidr     = var.cluster_vpc_cidr
  kubernetes_version   = var.kubernetes_version

  satellite_region     = var.satellite_region
  vpc_id               = module.satellite_vpc.vpc_id
  subnet_ids           = module.satellite_vpc.private_subnet_ids

  instance_type         = var.node_instance_type
  instance_profile_name = module.iam.instance_profile_name
  desired_capacity      = var.asg_desired_capacity
  max_size              = var.asg_max_size
  xrn_install_url       = var.xrn_install_url

  depends_on = [module.transit_gateway]
}
