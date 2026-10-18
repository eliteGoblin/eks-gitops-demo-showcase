locals {
  issuer = "https://auth.${var.domain}/realms/demo" # the realm in the gitops repo's config/keycloak/realm.yaml
}

# Access signs users in as an OIDC client of Keycloak (client
# "cloudflare-access"). Terraform owns the client secret: it generates it,
# configures Access with it and writes it to Secrets Manager, where the realm
# config in the gitops repo reads it. Secret name and JSON keys are that
# contract; change neither without the gitops repo.
resource "random_password" "access_client_secret" {
  length  = 48
  special = false
}

resource "aws_secretsmanager_secret" "access_client" {
  name                    = "demo/keycloak/cloudflare-access"
  description             = "Keycloak client cloudflare-access used by Cloudflare Access: client_id, client_secret; written by infra/cloudflare"
  recovery_window_in_days = 0 # demo: allow immediate re-create after destroy
}

resource "aws_secretsmanager_secret_version" "access_client" {
  secret_id = aws_secretsmanager_secret.access_client.id
  secret_string = jsonencode({
    client_id     = "cloudflare-access"
    client_secret = random_password.access_client_secret.result
  })
}

# Users need an email claim: every realm user has a verified email, and the
# "email" scope asks Keycloak to put it in the ID token.
resource "cloudflare_zero_trust_access_identity_provider" "keycloak" {
  account_id = var.account_id
  name       = "Keycloak"
  type       = "oidc"
  config = {
    client_id        = "cloudflare-access"
    client_secret    = random_password.access_client_secret.result
    auth_url         = "${local.issuer}/protocol/openid-connect/auth"
    token_url        = "${local.issuer}/protocol/openid-connect/token"
    certs_url        = "${local.issuer}/protocol/openid-connect/certs"
    scopes           = ["openid", "email", "profile"]
    email_claim_name = "email"
    pkce_enabled     = true
  }
}

# Access is the outer door, not the role check: anyone who signs in through
# Keycloak passes (the admins and demo.guest alike), then Argo CD and Grafana
# apply their own roles (demo.guest is read-only in both). The realm has no
# self-registration, so "signed in with Keycloak" means "a realm user".
resource "cloudflare_zero_trust_access_policy" "keycloak_users" {
  account_id = var.account_id
  name       = "Signed in with Keycloak"
  decision   = "allow"
  include    = [{ login_method = { id = cloudflare_zero_trust_access_identity_provider.keycloak.id } }]
}

# Only argocd. and grafana., never auth. (the Access login itself happens there,
# so it would loop) or app. (the API checks its own tokens). One application per
# host, with the host in every routed domain as a destination. Cloudflare
# accepts a destination on an active zone only, which is why a domain joins
# var.routed_domains after its name servers point at Cloudflare; the tunnel
# routes a host only after its destination exists (tunnel.tf, depends_on).
# Keycloak is the only identity provider offered (no one-time PIN), and the
# browser goes straight to its login page.
resource "cloudflare_zero_trust_access_application" "this" {
  for_each                  = { for host, cfg in local.hosts : host => cfg if cfg.access }
  account_id                = var.account_id
  name                      = "${each.key}.${var.domain}"
  type                      = "self_hosted"
  domain                    = "${each.key}.${var.domain}"
  destinations              = [for domain in var.routed_domains : { type = "public", uri = "${each.key}.${domain}" }]
  session_duration          = "8h"
  allowed_idps              = [cloudflare_zero_trust_access_identity_provider.keycloak.id]
  auto_redirect_to_identity = true
  policies                  = [{ id = cloudflare_zero_trust_access_policy.keycloak_users.id, precedence = 1 }]
}

# Paths of a gated host that Access must let through without a login: the more
# specific application wins over the host's own. For grafana. these are its
# externally shared dashboards; Grafana itself answers only for dashboards that
# have an enabled public link.
resource "cloudflare_zero_trust_access_policy" "public_paths" {
  account_id = var.account_id
  name       = "Public paths (bypass)"
  decision   = "bypass"
  include    = [{ everyone = {} }]
}

resource "cloudflare_zero_trust_access_application" "public" {
  for_each     = { for host, cfg in local.hosts : host => cfg if length(cfg.public_paths) > 0 }
  account_id   = var.account_id
  name         = "${each.key}.${var.domain} public paths"
  type         = "self_hosted"
  domain       = "${each.key}.${var.domain}${each.value.public_paths[0]}"
  destinations = [for pair in setproduct(var.routed_domains, each.value.public_paths) : { type = "public", uri = "${each.key}.${pair[0]}${pair[1]}" }]
  policies     = [{ id = cloudflare_zero_trust_access_policy.public_paths.id, precedence = 1 }]
}
