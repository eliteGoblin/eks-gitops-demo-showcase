locals {
  argocd_host = "argocd.${var.domain}"
  issuer      = "https://auth.${var.domain}/realms/demo"

  # Lets app-of-apps sync waves wait for child Applications to become healthy
  # (Argo CD dropped the built-in Application health check in 1.8).
  application_health = <<-LUA
    hs = {}
    hs.status = "Progressing"
    hs.message = ""
    if obj.status ~= nil and obj.status.health ~= nil then
      hs.status = obj.status.health.status
      if obj.status.health.message ~= nil then hs.message = obj.status.health.message end
    end
    return hs
  LUA
}

resource "helm_release" "argocd" {
  name             = "argocd"
  namespace        = "argocd"
  create_namespace = true
  repository       = "https://argoproj.github.io/argo-helm"
  chart            = "argo-cd"
  version          = var.argocd_chart_version
  timeout          = 900

  values = [yamlencode({
    global = { domain = local.argocd_host }
    configs = {
      params = { "server.insecure" = true } # TLS ends at the Cloudflare edge; cloudflared connects over HTTP
      cm = {
        "admin.enabled" = var.local_admin_enabled
        url             = "https://${local.argocd_host}"
        "oidc.config" = yamlencode({
          name            = "Keycloak"
          issuer          = local.issuer
          clientID        = "argocd"
          clientSecret    = "$argocd-oidc:clientSecret" # Secret synced by external-secrets
          requestedScopes = ["openid", "profile", "email", "groups"]
        })
        "resource.customizations.health.argoproj.io_Application" = local.application_health
      }
      rbac = {
        "policy.default" = ""
        scopes           = "[groups]"
        "policy.csv"     = <<-CSV
          g, argocd-admins, role:admin
          g, argocd-readonly, role:readonly
        CSV
      }
    }
  })]
}

# The bridge: Terraform outputs for the ApplicationSets (cluster generator).
resource "kubernetes_secret_v1" "in_cluster" {
  metadata {
    name      = "in-cluster"
    namespace = "argocd"
    labels = {
      "argocd.argoproj.io/secret-type" = "cluster"
      enable_external_secrets          = "true"
      enable_metrics_server            = "true"
      enable_reloader                  = "true"
      enable_observability             = "true"
      environment                      = "dev"
    }
    annotations = {
      cluster_name          = local.eks.cluster_name
      aws_region            = local.eks.region
      domain                = var.domain
      rds_endpoint          = local.eks.rds_endpoint
      rds_master_secret_arn = local.eks.rds_master_secret_arn
      amp_workspace_id      = local.eks.amp_workspace_id
      amp_endpoint          = local.eks.amp_endpoint
      apps_log_group        = local.eks.apps_log_group
      alerts_topic_arn      = local.eks.alerts_topic_arn
    }
  }
  data = {
    name   = "in-cluster"
    server = "https://kubernetes.default.svc"
  }
  depends_on = [helm_release.argocd]
}

# The root Application (app of apps), via the argocd-apps chart so Terraform
# needs no Argo CD CRDs at plan time.
resource "helm_release" "root" {
  name       = "root"
  namespace  = "argocd"
  repository = "https://argoproj.github.io/argo-helm"
  chart      = "argocd-apps"
  version    = var.argocd_apps_chart_version

  values = [yamlencode({
    applications = {
      root = {
        namespace = "argocd"
        project   = "default"
        source = {
          repoURL        = var.gitops_repo_url
          targetRevision = "main"
          path           = "bootstrap"
        }
        destination = { server = "https://kubernetes.default.svc", namespace = "argocd" }
        syncPolicy  = { automated = { prune = true, selfHeal = true } }
      }
    }
  })]
  depends_on = [kubernetes_secret_v1.in_cluster]
}
