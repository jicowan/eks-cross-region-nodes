terraform {
  required_providers {
    aws = {
      source                = "hashicorp/aws"
      configuration_aliases = [aws, aws.satellite]
    }
  }
}

# =============================================================================
# Transit Gateway — Cross-Region Peering Module
#
# Creates a TGW in each region, peers them, and injects routes into both VPC
# route tables so traffic between cluster and satellite VPCs flows through TGW.
# Peering must be accepted before routes are created (depends_on enforced).
# =============================================================================

# =============================================================================
# Transit Gateway — Cluster Region
# =============================================================================

resource "aws_ec2_transit_gateway" "cluster" {
  description = "${var.cluster_name}-tgw-cluster"

  default_route_table_association = "enable"
  default_route_table_propagation = "enable"

  tags = {
    Name = "${var.cluster_name}-tgw-cluster"
  }
}

resource "aws_ec2_transit_gateway_vpc_attachment" "cluster" {
  transit_gateway_id = aws_ec2_transit_gateway.cluster.id
  vpc_id             = var.cluster_vpc_id
  subnet_ids         = var.cluster_private_subnet_ids

  tags = {
    Name = "${var.cluster_name}-tgw-attach-cluster"
  }
}

# =============================================================================
# Transit Gateway — Satellite Region
# =============================================================================

resource "aws_ec2_transit_gateway" "satellite" {
  provider = aws.satellite

  description = "${var.cluster_name}-tgw-satellite"

  default_route_table_association = "enable"
  default_route_table_propagation = "enable"

  tags = {
    Name = "${var.cluster_name}-tgw-satellite"
  }
}

resource "aws_ec2_transit_gateway_vpc_attachment" "satellite" {
  provider = aws.satellite

  transit_gateway_id = aws_ec2_transit_gateway.satellite.id
  vpc_id             = var.satellite_vpc_id
  subnet_ids         = var.satellite_private_subnet_ids

  tags = {
    Name = "${var.cluster_name}-tgw-attach-satellite"
  }
}

# =============================================================================
# Cross-Region TGW Peering
# =============================================================================

resource "aws_ec2_transit_gateway_peering_attachment" "cross_region" {
  transit_gateway_id      = aws_ec2_transit_gateway.cluster.id
  peer_transit_gateway_id = aws_ec2_transit_gateway.satellite.id
  peer_region             = var.satellite_region

  tags = {
    Name = "${var.cluster_name}-tgw-peering"
  }
}

resource "aws_ec2_transit_gateway_peering_attachment_accepter" "cross_region" {
  provider = aws.satellite

  transit_gateway_attachment_id = aws_ec2_transit_gateway_peering_attachment.cross_region.id

  tags = {
    Name = "${var.cluster_name}-tgw-peering-accepter"
  }
}

# =============================================================================
# TGW Route Tables — Static Routes for Cross-Region Traffic
# =============================================================================

# Cluster TGW: route satellite CIDR → peering attachment
resource "aws_ec2_transit_gateway_route" "cluster_to_satellite" {
  destination_cidr_block         = var.satellite_vpc_cidr
  transit_gateway_attachment_id  = aws_ec2_transit_gateway_peering_attachment.cross_region.id
  transit_gateway_route_table_id = aws_ec2_transit_gateway.cluster.association_default_route_table_id

  depends_on = [aws_ec2_transit_gateway_peering_attachment_accepter.cross_region]
}

# Satellite TGW: route cluster CIDR → peering attachment
resource "aws_ec2_transit_gateway_route" "satellite_to_cluster" {
  provider = aws.satellite

  destination_cidr_block         = var.cluster_vpc_cidr
  transit_gateway_attachment_id  = aws_ec2_transit_gateway_peering_attachment_accepter.cross_region.id
  transit_gateway_route_table_id = aws_ec2_transit_gateway.satellite.association_default_route_table_id

  depends_on = [aws_ec2_transit_gateway_peering_attachment_accepter.cross_region]
}

# =============================================================================
# VPC Route Tables — Point cross-region traffic to TGW
# =============================================================================

# Cluster VPC: route satellite CIDR → cluster TGW
resource "aws_route" "cluster_to_satellite" {
  count = length(var.cluster_route_table_ids)

  route_table_id         = var.cluster_route_table_ids[count.index]
  destination_cidr_block = var.satellite_vpc_cidr
  transit_gateway_id     = aws_ec2_transit_gateway.cluster.id

  depends_on = [aws_ec2_transit_gateway_vpc_attachment.cluster]
}

# Satellite VPC: route cluster CIDR → satellite TGW
resource "aws_route" "satellite_to_cluster" {
  provider = aws.satellite
  count    = length(var.satellite_route_table_ids)

  route_table_id         = var.satellite_route_table_ids[count.index]
  destination_cidr_block = var.cluster_vpc_cidr
  transit_gateway_id     = aws_ec2_transit_gateway.satellite.id

  depends_on = [aws_ec2_transit_gateway_vpc_attachment.satellite]
}
