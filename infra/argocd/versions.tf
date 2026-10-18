# The GitOps bridge: the only place Terraform touches Kubernetes. It installs
# Argo CD, the root Application and the in-cluster "cluster" Secret whose
# annotations carry Terraform outputs to the ApplicationSets. Separate root so
# the Helm/Kubernetes providers configure against a cluster that already exists.
terraform {
  required_version = ">= 1.11"
  required_providers {
    aws        = { source = "hashicorp/aws", version = "~> 6.0" }
    helm       = { source = "hashicorp/helm", version = "~> 3.3" }
    kubernetes = { source = "hashicorp/kubernetes", version = "~> 3.3" }
  }
  backend "s3" {
    bucket       = "eks-gitops-demo-tfstate-852544369865"
    key          = "argocd/terraform.tfstate"
    region       = "ap-southeast-2"
    encrypt      = true
    use_lockfile = true
  }
}

data "terraform_remote_state" "eks" {
  backend = "s3"
  config  = { bucket = "eks-gitops-demo-tfstate-852544369865", key = "eks/terraform.tfstate", region = "ap-southeast-2" }
}

locals {
  eks = data.terraform_remote_state.eks.outputs
  exec = {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", local.eks.cluster_name, "--region", local.eks.region]
  }
}

provider "aws" {
  region = "ap-southeast-2"
}

provider "kubernetes" {
  host                   = local.eks.cluster_endpoint
  cluster_ca_certificate = base64decode(local.eks.cluster_certificate_authority_data)
  exec {
    api_version = local.exec.api_version
    command     = local.exec.command
    args        = local.exec.args
  }
}

provider "helm" {
  kubernetes = {
    host                   = local.eks.cluster_endpoint
    cluster_ca_certificate = base64decode(local.eks.cluster_certificate_authority_data)
    exec                   = local.exec
  }
}
