# CI roles for the platform pipelines. Same model as ci-ecr-push: GitHub OIDC,
# subject pinned to this repository's immutable ids and to exactly one workflow
# file, so no other workflow (even one added by a PR) can borrow the role.
#
#   ci-terraform-plan   tf-plan.yaml on pull requests: read-only plan, no lock
#   ci-terraform-apply  tf-apply.yaml on main:          plan + apply, holds the lock
#   ci-secrets-sync     secrets-sync.yaml on main:      sops decrypt → Secrets Manager
locals {
  workflow_ref = "job_workflow_ref:${var.github_repo}/.github/workflows"
  ci_subjects = {
    # pull_request runs carry the merge ref of the PR, so the PR number is a wildcard (StringLike).
    terraform_plan  = "repo:${var.github_repo_subject}:pull_request:${local.workflow_ref}/tf-plan.yaml@refs/pull/*/merge"
    terraform_apply = "repo:${var.github_repo_subject}:ref:refs/heads/main:${local.workflow_ref}/tf-apply.yaml@refs/heads/main"
    secrets_sync    = "repo:${var.github_repo_subject}:ref:refs/heads/main:${local.workflow_ref}/secrets-sync.yaml@refs/heads/main"
  }
}

data "aws_iam_policy_document" "ci_oidc_trust" {
  for_each = local.ci_subjects
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = each.key == "terraform_plan" ? "StringLike" : "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = [each.value]
    }
  }
}

# ---- plan (pull requests): read everything, write nothing, decrypt nothing ----
# PR workflows run proposed code, so this role must not be able to read secret
# material even though ReadOnlyAccess alone already excludes GetSecretValue.
resource "aws_iam_role" "ci_terraform_plan" {
  name                 = "ci-terraform-plan"
  assume_role_policy   = data.aws_iam_policy_document.ci_oidc_trust["terraform_plan"].json
  max_session_duration = 3600
}

resource "aws_iam_role_policy_attachment" "ci_terraform_plan_readonly" {
  role       = aws_iam_role.ci_terraform_plan.name
  policy_arn = "arn:aws:iam::aws:policy/ReadOnlyAccess"
}

data "aws_iam_policy_document" "ci_terraform_plan_deny_secrets" {
  statement {
    sid    = "NoSecretMaterialFromPullRequests"
    effect = "Deny"
    actions = [
      "secretsmanager:GetSecretValue",
      "kms:Decrypt",
      "ec2:GetPasswordData",
      "eks:AccessKubernetesApi",
    ]
    resources = ["*"]
  }
  # Only this account's parameters (SecureStrings live there). The public
  # /aws/service/* parameters have no account in their ARN and stay readable:
  # the EKS module reads the recommended AMI release version from one.
  statement {
    sid       = "NoAccountParametersFromPullRequests"
    effect    = "Deny"
    actions   = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
    resources = ["arn:aws:ssm:*:${data.aws_caller_identity.current.account_id}:parameter/*"]
  }
  # The cloudflare root's state holds secret values (the tunnel token, the
  # Access client secret), unlike the other roots' state. Pull requests only
  # run its static checks, which init without the backend (scripts/ci/tf.sh).
  # The second ARN covers the same key under a workspace prefix.
  statement {
    sid     = "NoCloudflareStateFromPullRequests"
    effect  = "Deny"
    actions = ["s3:GetObject", "s3:GetObjectVersion"]
    resources = [
      "${aws_s3_bucket.tfstate.arn}/cloudflare/*",
      "${aws_s3_bucket.tfstate.arn}/env:/*/cloudflare/*",
    ]
  }
}

resource "aws_iam_role_policy" "ci_terraform_plan_deny_secrets" {
  name   = "deny-secret-material"
  role   = aws_iam_role.ci_terraform_plan.id
  policy = data.aws_iam_policy_document.ci_terraform_plan_deny_secrets.json
}

# ---- apply (main): manages EKS, IAM, RDS, … ----
# Known risk: AdministratorAccess. The boundary is the
# trust policy (one workflow, one branch, one repository id); a permissions
# boundary or a per-root least-privilege policy is the production follow-up.
resource "aws_iam_role" "ci_terraform_apply" {
  name                 = "ci-terraform"
  assume_role_policy   = data.aws_iam_policy_document.ci_oidc_trust["terraform_apply"].json
  max_session_duration = 3600
}

resource "aws_iam_role_policy_attachment" "ci_terraform_apply_admin" {
  role       = aws_iam_role.ci_terraform_apply.name
  policy_arn = "arn:aws:iam::aws:policy/AdministratorAccess"
}

# ---- secrets sync (main): sops-encrypted files in Git → Secrets Manager ----
# Decrypts only with the sops key and writes only the sops-managed namespace
# demo/<env>/<service>/<key>; the long-lived platform secrets (demo/keycloak/*,
# demo/argocd/*) are out of its reach.
resource "aws_iam_role" "ci_secrets_sync" {
  name                 = "ci-secrets-sync"
  assume_role_policy   = data.aws_iam_policy_document.ci_oidc_trust["secrets_sync"].json
  max_session_duration = 3600
}

data "aws_iam_policy_document" "ci_secrets_sync" {
  statement {
    sid       = "DecryptWithTheSopsKey"
    actions   = ["kms:Decrypt", "kms:DescribeKey"]
    resources = [aws_kms_key.sops.arn]
  }
  statement {
    sid = "SyncSopsManagedSecrets"
    actions = [
      "secretsmanager:CreateSecret",
      "secretsmanager:DescribeSecret",
      "secretsmanager:GetSecretValue",
      "secretsmanager:PutSecretValue",
      "secretsmanager:TagResource",
    ]
    resources = [
      "arn:aws:secretsmanager:${var.region}:${data.aws_caller_identity.current.account_id}:secret:demo/dev/*",
      "arn:aws:secretsmanager:${var.region}:${data.aws_caller_identity.current.account_id}:secret:demo/prod/*",
    ]
  }
}

resource "aws_iam_role_policy" "ci_secrets_sync" {
  name   = "sops-to-secrets-manager"
  role   = aws_iam_role.ci_secrets_sync.id
  policy = data.aws_iam_policy_document.ci_secrets_sync.json
}
