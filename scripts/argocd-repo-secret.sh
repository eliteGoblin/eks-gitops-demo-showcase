#!/usr/bin/env bash
# Give Argo CD read access to the private gitops repo. The deploy key lives in
# Secrets Manager (seeded by secrets-seed.sh); it is piped straight into a
# Kubernetes Secret so it never lands in Terraform state. This is the one
# imperative bootstrap step: external-secrets is installed *from* that repo,
# so it cannot provide the credential that lets Argo CD read it.
set -euo pipefail
REGION=${AWS_REGION:-ap-southeast-2}
REPO_URL=${GITOPS_REPO_URL:-git@github.com:eliteGoblin/eks-gitops-demo-gitops.git}
key=$(aws secretsmanager get-secret-value --region "$REGION" --secret-id demo/argocd/gitops-repo \
        --query SecretString --output text | jq -r .sshPrivateKey)
kubectl -n argocd create secret generic gitops-repo \
  --from-literal=type=git --from-literal=url="$REPO_URL" --from-literal=sshPrivateKey="$key" \
  --dry-run=client -o yaml \
  | kubectl label --local -f - argocd.argoproj.io/secret-type=repository -o yaml \
  | kubectl apply -f -
echo "Argo CD can read $REPO_URL"
