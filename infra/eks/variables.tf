variable "region" {
  type    = string
  default = "ap-southeast-2"
}

variable "name" {
  description = "Cluster and resource name prefix."
  type        = string
  default     = "eks-gitops-demo"
}

variable "kubernetes_version" {
  description = "EKS version: the newest in standard support when the cluster was built."
  type        = string
  default     = "1.37"
}

variable "api_allowed_cidrs" {
  description = <<-EOT
    CIDRs allowed to reach the public EKS API endpoint. Access is still IAM-authenticated.
    Open by default because the admin laptop's IP changes (phone hotspot); narrow it for real use.
  EOT
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "node_instance_types" {
  type    = list(string)
  default = ["t4g.large"]
}

variable "alerts_email" {
  description = "Recipient of the SNS alert topic (must confirm the subscription email once)."
  type        = string
  default     = "alerts@example.com"
}
