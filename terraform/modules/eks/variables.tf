variable "cluster_name" {
  description = "Name of the EKS cluster"
  type        = string
}

variable "kubernetes_version" {
  description = "Kubernetes version"
  type        = string
}

variable "subnet_ids" {
  description = "Subnet IDs for the EKS cluster ENIs"
  type        = list(string)
}

variable "vpc_id" {
  description = "VPC ID for the cluster"
  type        = string
}

variable "satellite_vpc_cidr" {
  description = "Satellite VPC CIDR (for security group rules)"
  type        = string
}
