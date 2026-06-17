output "node_group_name" {
  description = "Managed node group name"
  value       = aws_eks_node_group.local.node_group_name
}

output "node_group_role_arn" {
  description = "Node group IAM role ARN"
  value       = aws_iam_role.node_group.arn
}
