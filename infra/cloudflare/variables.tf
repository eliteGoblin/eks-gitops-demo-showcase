variable "region" {
  description = "AWS region of the state bucket and the secrets."
  type        = string
  default     = "ap-southeast-2"
}

variable "account_id" {
  description = "Cloudflare account that owns the zones, the tunnel and Access (an identifier, not a secret)."
  type        = string
  default     = "02de92389bcc34a145ddde2f0924fc71"
}

variable "domain" {
  description = <<-EOT
    The domain the demo is served on: Keycloak's issuer is https://auth.<domain>/realms/demo
    (the Access identity provider dials it) and the Access applications are named after its
    hosts. One of var.zones and var.routed_domains.
  EOT
  type        = string
  default     = "frankbluemind.com"
}

variable "zones" {
  description = <<-EOT
    Every zone this root serves (the registrar's name servers point at Cloudflare). Each
    holds the four demo hosts as proxied CNAMEs to the tunnel; frankbluemind.com also keeps
    the records it had at GoDaddy (dns-frankbluemind.tf). It joined because corporate web
    filters block eksgitops.click as a newly registered domain.
  EOT
  type        = set(string)
  default     = ["eksgitops.click", "frankbluemind.com"]

  validation {
    condition     = contains(var.zones, var.domain)
    error_message = "zones must include var.domain."
  }
}

variable "routed_domains" {
  description = <<-EOT
    Domains whose four hosts the tunnel routes to the in-cluster Services and whose argocd.
    and grafana. are destinations of the Access applications, in ingress order. A domain
    joins only once its zone is active (Access refuses a destination on a pending zone) and
    leaves once its hosts redirect elsewhere. Always lists var.domain.
  EOT
  type        = list(string)
  default     = ["frankbluemind.com"]

  validation {
    condition = (
      contains(var.routed_domains, var.domain) &&
      length(var.routed_domains) == length(distinct(var.routed_domains)) &&
      alltrue([for d in var.routed_domains : contains(var.zones, d)])
    )
    error_message = "routed_domains must list var.domain and only zones of var.zones, each once."
  }
}

variable "redirect_domains" {
  description = <<-EOT
    Zones whose hosts redirect (301) to the same host under var.domain: the demo's previous
    domains, kept so that old links keep working (redirects.tf). Zones of var.zones that are
    neither var.domain nor routed.
  EOT
  type        = list(string)
  default     = ["eksgitops.click"]

  validation {
    condition     = alltrue([for d in var.redirect_domains : contains(var.zones, d) && d != var.domain && !contains(var.routed_domains, d)])
    error_message = "redirect_domains must be zones of var.zones other than var.domain and the routed domains."
  }
}

variable "access_team_domain" {
  description = <<-EOT
    Zero Trust team domain. The organization was created once through the API and is
    not managed here (nothing in it differs from the defaults); only its callback URL is used.
  EOT
  type        = string
  default     = "eksgitops.cloudflareaccess.com"
}
