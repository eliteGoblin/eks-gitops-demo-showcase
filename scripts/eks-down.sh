#!/usr/bin/env bash
# Tear down the cluster layer; the bootstrap layer (state, ECR, long-lived
# secrets) and the Cloudflare layer are untouched. No controller in the cluster
# creates load balancers any more, so nothing outside Terraform blocks the VPC
# deletion.
set -euo pipefail
CLUSTER=eks-gitops-demo
REGION=${AWS_REGION:-ap-southeast-2}
if aws eks describe-cluster --region "$REGION" --name "$CLUSTER" >/dev/null 2>&1; then
  aws eks update-kubeconfig --region "$REGION" --name "$CLUSTER" --alias "$CLUSTER" >/dev/null
  # Stop Argo CD from re-creating what the destroy removes.
  kubectl -n argocd delete application root --ignore-not-found --wait=false || true
  kubectl -n argocd patch application --all --type merge -p '{"spec":{"syncPolicy":null}}' 2>/dev/null || true
  terraform -chdir=infra/argocd destroy -input=false -auto-approve || true
fi
terraform -chdir=infra/eks destroy -input=false -auto-approve
