variable "domain" {
  description = "The demo's domain: Argo CD's url and issuer, and the cluster Secret's annotation that the gitops ApplicationSets read (Keycloak's hostname, Grafana's URLs)."
  type        = string
  default     = "frankbluemind.com"
}

variable "gitops_repo_url" {
  type    = string
  default = "git@github.com:eliteGoblin/eks-gitops-demo-gitops.git"
}

variable "argocd_chart_version" {
  type    = string
  default = "10.10.5" # Argo CD v3.5.5
}

variable "argocd_apps_chart_version" {
  type    = string
  default = "2.0.6"
}

variable "local_admin_enabled" {
  description = "Argo CD built-in admin account. Off: everyone logs in through Keycloak. Break-glass: apply with -var local_admin_enabled=true, then read the password from the argocd-initial-admin-secret Secret."
  type        = bool
  default     = false
}
