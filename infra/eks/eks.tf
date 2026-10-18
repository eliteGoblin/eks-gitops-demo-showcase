locals {
  root_arn = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:root"
  cluster_admin_association = {
    policy_arn   = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
    access_scope = { type = "cluster" }
  }
}

module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 21.29"

  name               = var.name
  kubernetes_version = var.kubernetes_version

  endpoint_public_access       = true
  endpoint_public_access_cidrs = var.api_allowed_cidrs
  # Access entries (API mode) instead of the aws-auth ConfigMap, listed
  # explicitly: the account root (the laptop; accepted demo shortcut, production
  # maps an admin role) and the IaC pipeline's apply role. Not the module's
  # "cluster creator" shortcut: that pins admin to whoever ran Terraform last,
  # so the first CI apply would have evicted the laptop.
  authentication_mode                      = "API"
  enable_cluster_creator_admin_permissions = false
  access_entries = {
    root = {
      principal_arn       = local.root_arn
      policy_associations = { admin = local.cluster_admin_association }
    }
    ci_terraform = {
      principal_arn       = local.bootstrap.ci_terraform_roles.apply
      policy_associations = { admin = local.cluster_admin_association }
    }
  }
  # Same two identities administer the cluster's secrets-encryption KMS key
  # (the module would otherwise pin it to the current caller as well).
  kms_key_administrators = [local.root_arn, local.bootstrap.ci_terraform_roles.apply]

  # Every workload gets AWS credentials through EKS Pod Identity, so the IAM
  # OIDC provider the module creates for IRSA was unused (and its thumbprint,
  # computed behind a TLS-inspecting proxy, was the one diff CI kept planning).
  enable_irsa = false

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.public_subnets

  # EKS managed add-ons are AWS API objects, so Terraform owns them. vpc-cni and
  # the Pod Identity agent go in before the nodes so pods get IPs and AWS
  # credentials from the first boot. vpc-cni also enforces NetworkPolicy
  # (eBPF node agent; the policy controller runs in the EKS control plane).
  addons = {
    vpc-cni = {
      before_compute       = true
      configuration_values = jsonencode({ enableNetworkPolicy = "true" })
    }
    eks-pod-identity-agent = { before_compute = true }
    coredns                = {}
    kube-proxy             = {}
  }

  eks_managed_node_groups = {
    default = {
      ami_type       = "AL2023_ARM_64_STANDARD"
      instance_types = var.node_instance_types
      capacity_type  = "ON_DEMAND" # idea: SPOT with 2+ instance types
      min_size       = 2
      max_size       = 4
      desired_size   = 2
      disk_size      = 30
    }
  }
}
