# Long-lived secret containers. Terraform creates only the name and policy;
# values are written by `make secrets-seed` (scripts/secrets-seed.sh), so no
# secret value ever lands in Terraform state. They outlive cluster rebuilds.
locals {
  secrets = {
    "demo/keycloak/admin"     = "Keycloak bootstrap admin (master realm): username, password"
    "demo/argocd/oidc"        = "Argo CD OIDC client secret shared by Keycloak and Argo CD: clientSecret"
    "demo/argocd/gitops-repo" = "Read-only deploy key Argo CD uses to pull the gitops repo: sshPrivateKey"
    "demo/load-generator"     = "Keycloak client secret of the k6 load generator (service account = demo participant): clientSecret"
    "demo/demo-api-svc"       = "Keycloak client secret of demo-api's service account, which calls payments-svc: clientSecret"
    "demo/keycloak/users"     = "Passwords of the demo users in realm demo, injected by the realm Job: one key per username"
  }
}

resource "aws_secretsmanager_secret" "this" {
  for_each                = local.secrets
  name                    = each.key
  description             = each.value
  recovery_window_in_days = 0 # demo: allow immediate re-create after destroy
}
