variable "cluster_name" {
  description = "EKS cluster name"
  type        = string
}

variable "cluster_region" {
  description = "Cluster region"
  type        = string
}

variable "cluster_endpoint" {
  description = "EKS cluster API endpoint"
  type        = string
}

variable "cluster_ca" {
  description = "EKS cluster certificate authority data"
  type        = string
}

variable "cluster_service_cidr" {
  description = "Kubernetes service CIDR"
  type        = string
}

variable "cluster_vpc_cidr" {
  description = "Cluster VPC CIDR (for satellite SG inbound rules)"
  type        = string
}

variable "kubernetes_version" {
  description = "Kubernetes version (for AMI lookup)"
  type        = string
}

variable "satellite_region" {
  description = "Satellite region"
  type        = string
}

variable "vpc_id" {
  description = "Satellite VPC ID"
  type        = string
}

variable "subnet_ids" {
  description = "Satellite private subnet IDs"
  type        = list(string)
}

variable "instance_type" {
  description = "EC2 instance type for satellite nodes"
  type        = string
  default     = "m6i.large"
}

variable "instance_profile_name" {
  description = "IAM instance profile name"
  type        = string
}

variable "desired_capacity" {
  description = "Desired number of satellite nodes"
  type        = number
  default     = 1
}

variable "max_size" {
  description = "Maximum number of satellite nodes"
  type        = number
  default     = 3
}

variable "xrn_install_url" {
  description = "Download URL for xrn-install binary"
  type        = string
}
