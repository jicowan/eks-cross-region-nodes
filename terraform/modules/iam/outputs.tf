output "node_role_arn" {
  description = "Cross-region node IAM role ARN"
  value       = aws_iam_role.node.arn
}

output "node_role_name" {
  description = "Cross-region node IAM role name"
  value       = aws_iam_role.node.name
}

output "instance_profile_name" {
  description = "Instance profile name for satellite nodes"
  value       = aws_iam_instance_profile.node.name
}
