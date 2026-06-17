output "cluster_tgw_id" {
  description = "Transit Gateway ID in the cluster region"
  value       = aws_ec2_transit_gateway.cluster.id
}

output "satellite_tgw_id" {
  description = "Transit Gateway ID in the satellite region"
  value       = aws_ec2_transit_gateway.satellite.id
}

output "peering_attachment_id" {
  description = "Cross-region peering attachment ID"
  value       = aws_ec2_transit_gateway_peering_attachment.cross_region.id
}
