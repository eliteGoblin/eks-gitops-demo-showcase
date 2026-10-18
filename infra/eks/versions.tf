terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
  backend "s3" {
    bucket       = "eks-gitops-demo-tfstate-852544369865"
    key          = "eks/terraform.tfstate"
    region       = "ap-southeast-2"
    encrypt      = true
    use_lockfile = true
  }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = {
      Project   = "eks-gitops-demo"
      Layer     = "eks"
      ManagedBy = "terraform"
    }
  }
}

# Long-lived outputs (secret ARNs, the CI apply role) from the bootstrap layer.
data "terraform_remote_state" "bootstrap" {
  backend = "s3"
  config = {
    bucket = "eks-gitops-demo-tfstate-852544369865"
    key    = "bootstrap/terraform.tfstate"
    region = "ap-southeast-2"
  }
}
