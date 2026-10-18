# GitHub Actions gets short-lived AWS credentials through OIDC federation: no
# stored keys. Only the release workflow, running for a push to main of this one
# repository, may assume the role, and the role may only push to this demo's
# ECR repositories. The repository's OIDC subject is customized to include
# job_workflow_ref (gh api .../actions/oidc/customization/sub, include_claim_keys
# ["repo", "context", "job_workflow_ref"], immutable subject), so another
# workflow on main (say, a compromised PR-added one) cannot get these credentials.
locals {
  release_subject = join(":", [
    "repo:${var.github_repo_subject}",
    "ref:refs/heads/main",
    "job_workflow_ref:${var.github_repo}/.github/workflows/release.yaml@refs/heads/main",
  ])
}
resource "aws_iam_openid_connect_provider" "github" {
  url            = "https://token.actions.githubusercontent.com"
  client_id_list = ["sts.amazonaws.com"]
}

data "aws_iam_policy_document" "ci_trust" {
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
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = [local.release_subject]
    }
  }
}

resource "aws_iam_role" "ci_ecr_push" {
  name                 = "ci-ecr-push"
  assume_role_policy   = data.aws_iam_policy_document.ci_trust.json
  max_session_duration = 3600
}

data "aws_iam_policy_document" "ci_ecr_push" {
  statement {
    sid       = "Login"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"] # GetAuthorizationToken does not support resource-level permissions
  }
  statement {
    sid = "PushToDemoRepositories"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchGetImage",
      "ecr:CompleteLayerUpload",
      "ecr:DescribeImages",
      "ecr:DescribeRepositories",
      "ecr:GetDownloadUrlForLayer",
      "ecr:InitiateLayerUpload",
      "ecr:PutImage",
      "ecr:UploadLayerPart",
    ]
    resources = [for r in aws_ecr_repository.this : r.arn]
  }
}

resource "aws_iam_role_policy" "ci_ecr_push" {
  name   = "ecr-push"
  role   = aws_iam_role.ci_ecr_push.id
  policy = data.aws_iam_policy_document.ci_ecr_push.json
}
