# Cloudflare in front of the demo (Phase 8): the zone and its DNS, the tunnel
# that cloudflared in the cluster dials out to, and Access with Keycloak as the
# identity provider. Long-lived, like bootstrap: cluster rebuilds don't touch it.
# Design and runbook: docs/eks-gitops-demo-cloudflare.html.
terraform {
  required_version = ">= 1.11"
  required_providers {
    aws        = { source = "hashicorp/aws", version = "~> 6.0" }
    cloudflare = { source = "cloudflare/cloudflare", version = "~> 5.28" }
    random     = { source = "hashicorp/random", version = "~> 3.9" }
  }
  backend "s3" {
    bucket       = "eks-gitops-demo-tfstate-852544369865"
    key          = "cloudflare/terraform.tfstate"
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
      Layer     = "cloudflare"
      ManagedBy = "terraform"
    }
  }
}

# Authenticates with CLOUDFLARE_API_TOKEN from the environment, never a file:
# tf-apply.yaml reads Secrets Manager demo/cloudflare/api-token and masks it.
# Pull requests never get the token, so they only run the static checks.
provider "cloudflare" {}
