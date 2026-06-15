variable "cluster_name" {
  description = "Name of the EKS cluster"
  type        = string
  default     = "xrn-demo"
}

variable "cluster_region" {
  description = "AWS region for the EKS cluster"
  type        = string
  default     = "us-west-2"
}

variable "satellite_region" {
  description = "AWS region for the satellite worker nodes"
  type        = string
  default     = "us-east-1"
}

variable "cluster_vpc_cidr" {
  description = "CIDR block for the cluster VPC"
  type        = string
  default     = "10.50.0.0/16"
}

variable "satellite_vpc_cidr" {
  description = "CIDR block for the satellite VPC"
  type        = string
  default     = "10.60.0.0/16"
}

variable "kubernetes_version" {
  description = "Kubernetes version for the EKS cluster"
  type        = string
  default     = "1.32"
}

variable "node_instance_type" {
  description = "EC2 instance type for satellite worker nodes"
  type        = string
  default     = "m6i.large"
}

variable "local_node_instance_type" {
  description = "EC2 instance type for local managed node group"
  type        = string
  default     = "t3.medium"
}

variable "local_node_count" {
  description = "Desired node count for local managed node group"
  type        = number
  default     = 2
}

variable "asg_desired_capacity" {
  description = "Desired number of satellite worker nodes"
  type        = number
  default     = 1
}

variable "asg_max_size" {
  description = "Maximum number of satellite worker nodes"
  type        = number
  default     = 3
}

variable "xrn_install_url" {
  description = "Download URL for the xrn-install binary"
  type        = string
  default     = "https://github.com/jicowan/eks-cross-region-nodes/releases/latest/download/xrn-install-linux-amd64"
}
