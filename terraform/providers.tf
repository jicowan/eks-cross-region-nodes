provider "aws" {
  region = var.cluster_region

  default_tags {
    tags = {
      Project   = "eks-cross-region-nodes"
      ManagedBy = "terraform"
    }
  }
}

provider "aws" {
  alias  = "satellite"
  region = var.satellite_region

  default_tags {
    tags = {
      Project   = "eks-cross-region-nodes"
      ManagedBy = "terraform"
    }
  }
}
