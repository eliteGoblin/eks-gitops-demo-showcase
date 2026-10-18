output "tfstate_bucket" {
  value = aws_s3_bucket.tfstate.bucket
}

output "ecr_repository_urls" {
  value = { for k, r in aws_ecr_repository.this : k => r.repository_url }
}

output "ci_ecr_push_role_arn" {
  value = aws_iam_role.ci_ecr_push.arn
}

output "secret_arns" {
  value = { for k, s in aws_secretsmanager_secret.this : k => s.arn }
}

output "ci_terraform_roles" {
  description = "GitHub OIDC roles of the platform pipelines (plan on PRs, apply on main, sops → Secrets Manager)."
  value = {
    plan         = aws_iam_role.ci_terraform_plan.arn
    apply        = aws_iam_role.ci_terraform_apply.arn
    secrets_sync = aws_iam_role.ci_secrets_sync.arn
  }
}

output "sops_kms_key_arn" {
  value = aws_kms_key.sops.arn
}
