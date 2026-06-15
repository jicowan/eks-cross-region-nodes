variable "cluster_name" {
  description = "EKS cluster name"
  type        = string
}

variable "cluster_version" {
  description = "EKS cluster Kubernetes version"
  type        = string
}

variable "subnet_ids" {
  description = "Private subnet IDs for the node group"
  type        = list(string)
}

variable "instance_type" {
  description = "EC2 instance type for local nodes"
  type        = string
  default     = "m6i.medium"
}

variable "desired_count" {
  description = "Desired number of local nodes"
  type        = number
  default     = 2
}
