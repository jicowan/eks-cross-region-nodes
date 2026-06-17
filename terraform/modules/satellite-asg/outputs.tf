output "asg_name" {
  description = "Auto Scaling group name"
  value       = aws_autoscaling_group.satellite.name
}

output "launch_template_id" {
  description = "Launch template ID"
  value       = aws_launch_template.satellite.id
}

output "satellite_security_group_id" {
  description = "Satellite node security group ID"
  value       = aws_security_group.satellite_node.id
}
