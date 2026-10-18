data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, 2)
}

# Two AZs. Nodes run in the public subnets with public IPs but no inbound
# rules, which avoids a NAT gateway (~US$45/month) for the demo. Production
# would place nodes in the private subnets behind NAT or VPC endpoints.
# The private subnets hold RDS only.
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "~> 6.7"

  name = var.name
  cidr = "10.40.0.0/16"
  azs  = local.azs

  public_subnets  = ["10.40.0.0/20", "10.40.16.0/20"]
  private_subnets = ["10.40.32.0/20", "10.40.48.0/20"]

  enable_nat_gateway      = false
  map_public_ip_on_launch = true
}
