variable "region" {
  description = "AWS region for every resource in the demo."
  type        = string
  default     = "ap-southeast-2"
}

variable "project" {
  description = "Name prefix for resources."
  type        = string
  default     = "eks-gitops-demo"
}

variable "github_repo_subject" {
  description = <<-EOT
    Repository part of the GitHub OIDC "sub" claim, in GitHub's immutable form
    owner@owner_id/repo@repo_id (see GET /repos/{owner}/{repo}/actions/oidc/customization/sub).
    Numeric ids survive renames, so a re-created repo with the same name cannot assume the role.
  EOT
  type        = string
  default     = "eliteGoblin@4377212/eks-gitops-demo@1421473999"
}

variable "github_repo" {
  description = "owner/repo, as it appears in the job_workflow_ref claim (name-based; the subject prefix pins the immutable ids)."
  type        = string
  default     = "eliteGoblin/eks-gitops-demo"
}
