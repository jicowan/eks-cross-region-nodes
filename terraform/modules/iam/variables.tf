variable "cluster_name" {
  description = "EKS cluster name"
  type        = string
}

variable "cluster_arn" {
  description = "EKS cluster ARN"
  type        = string
}

variable "node_role_name" {
  description = "Name for the cross-region node IAM role"
  type        = string
}
