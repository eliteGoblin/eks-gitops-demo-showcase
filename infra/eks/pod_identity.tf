# One IAM role per controller, bound to its namespace/service account through
# EKS Pod Identity (no per-cluster OIDC provider, no role annotations).
locals {
  bootstrap   = data.terraform_remote_state.bootstrap.outputs
  secret_arns = values(local.bootstrap.secret_arns)
}

# external-secrets reads the demo secrets and the RDS-managed master secret,
# and mints ECR tokens (Argo CD pulls the Helm chart from ECR).
module "external_secrets_identity" {
  source  = "terraform-aws-modules/eks-pod-identity/aws"
  version = "~> 2.9"

  name                           = "${var.name}-external-secrets"
  attach_external_secrets_policy = true
  external_secrets_secrets_manager_arns = concat(local.secret_arns, [
    aws_db_instance.this.master_user_secret[0].secret_arn,
    # Secrets authored as code (secrets/dev/*.enc.yaml), synced by CI (secrets-sync.yaml).
    "arn:aws:secretsmanager:${var.region}:${data.aws_caller_identity.current.account_id}:secret:demo/dev/*",
    # Written by infra/cloudflare: the tunnel token (cloudflared) and the Keycloak
    # client secret of Cloudflare Access. Exact names plus Secrets Manager's
    # 6-character suffix, never demo/cloudflare/*, which holds the API token too.
    "arn:aws:secretsmanager:${var.region}:${data.aws_caller_identity.current.account_id}:secret:demo/cloudflare/tunnel-token-??????",
    "arn:aws:secretsmanager:${var.region}:${data.aws_caller_identity.current.account_id}:secret:demo/keycloak/cloudflare-access-??????",
  ])
  external_secrets_create_permission = false

  attach_custom_policy = true
  # The ECR token it mints carries this role's permissions, so it may also
  # read (pull) the Helm chart repositories, and nothing else.
  policy_statements = [
    {
      sid       = "EcrAuthToken"
      actions   = ["ecr:GetAuthorizationToken"]
      resources = ["*"]
    },
    {
      sid       = "PullHelmChart"
      actions   = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability", "ecr:DescribeImages"]
      resources = [for chart in ["demo-api", "payments-svc"] : "arn:aws:ecr:${var.region}:${data.aws_caller_identity.current.account_id}:repository/charts/${chart}"]
    },
  ]

  associations = {
    main = {
      cluster_name    = module.eks.cluster_name
      namespace       = "external-secrets"
      service_account = "external-secrets"
    }
  }
}

data "aws_caller_identity" "current" {}
