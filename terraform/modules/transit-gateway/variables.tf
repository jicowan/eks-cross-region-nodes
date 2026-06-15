variable "cluster_name" {
  description = "Cluster name (for resource naming)"
  type        = string
}

variable "cluster_vpc_id" {
  description = "Cluster VPC ID"
  type        = string
}

variable "cluster_vpc_cidr" {
  description = "Cluster VPC CIDR"
  type        = string
}

variable "cluster_private_subnet_ids" {
  description = "Cluster private subnet IDs for TGW attachment"
  type        = list(string)
}

variable "cluster_route_table_ids" {
  description = "Cluster private route table IDs (for TGW route injection)"
  type        = list(string)
}

variable "satellite_vpc_id" {
  description = "Satellite VPC ID"
  type        = string
}

variable "satellite_vpc_cidr" {
  description = "Satellite VPC CIDR"
  type        = string
}

variable "satellite_private_subnet_ids" {
  description = "Satellite private subnet IDs for TGW attachment"
  type        = list(string)
}

variable "satellite_route_table_ids" {
  description = "Satellite private route table IDs (for TGW route injection)"
  type        = list(string)
}

variable "cluster_region" {
  description = "Cluster region"
  type        = string
}

variable "satellite_region" {
  description = "Satellite region"
  type        = string
}
