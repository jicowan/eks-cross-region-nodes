output "cluster_name" {
  description = "EKS cluster name"
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "EKS cluster API endpoint"
  value       = module.eks.cluster_endpoint
}

output "cluster_region" {
  description = "Cluster region"
  value       = var.cluster_region
}

output "satellite_region" {
  description = "Satellite region"
  value       = var.satellite_region
}

output "satellite_vpc_id" {
  description = "Satellite VPC ID (pass to xrnctl add-satellite)"
  value       = module.satellite_vpc.vpc_id
}

output "satellite_asg_name" {
  description = "Satellite Auto Scaling group name"
  value       = module.satellite_asg.asg_name
}

output "node_role_arn" {
  description = "Cross-region node IAM role ARN"
  value       = module.iam.node_role_arn
}

output "instance_profile_name" {
  description = "Instance profile name for satellite nodes"
  value       = module.iam.instance_profile_name
}

output "xrnctl_add_satellite_command" {
  description = "Command to run after terraform apply"
  value       = <<-EOT
    xrnctl add-satellite \
      --cluster-name ${module.eks.cluster_name} --cluster-region ${var.cluster_region} \
      --vpc-id ${module.satellite_vpc.vpc_id} --satellite-region ${var.satellite_region}
  EOT
}

output "xrnctl_verify_command" {
  description = "Command to verify satellite registration"
  value       = "xrnctl verify --cluster-name ${module.eks.cluster_name} --cluster-region ${var.cluster_region}"
}
