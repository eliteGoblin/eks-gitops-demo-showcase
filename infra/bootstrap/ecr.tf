# Container image and Helm chart (OCI) registries. Tags are immutable: every
# build gets a new tag, so an environment pins exact bytes.
locals {
  repositories = ["demo-api", "charts/demo-api", "payments-svc", "charts/payments-svc"]
}

resource "aws_ecr_repository" "this" {
  for_each             = toset(local.repositories)
  name                 = each.value
  image_tag_mutability = "IMMUTABLE"
  force_delete         = true # demo: allow terraform destroy with images inside

  image_scanning_configuration {
    scan_on_push = true
  }
  encryption_configuration {
    encryption_type = "AES256"
  }
}

# Age-based retention: a GitOps revert must find the artifact it points at.
# A count-based rule would be eaten by cosign signatures and SBOM attestations
# (stored next to each image and chart), so keep everything for 90 days.
resource "aws_ecr_lifecycle_policy" "retention" {
  for_each   = aws_ecr_repository.this
  repository = each.value.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Expire artifacts pushed more than 90 days ago"
      selection    = { tagStatus = "any", countType = "sinceImagePushed", countUnit = "days", countNumber = 90 }
      action       = { type = "expire" }
    }]
  })
}

moved {
  from = aws_ecr_lifecycle_policy.keep_recent
  to   = aws_ecr_lifecycle_policy.retention
}
