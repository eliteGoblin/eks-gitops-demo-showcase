# A remotely managed tunnel: cloudflared in the cluster runs with nothing but
# a token, and its routes (ingress rules) live here, one per host and routed
# domain; every host's DNS record points at the tunnel (dns.tf).
resource "cloudflare_zero_trust_tunnel_cloudflared" "eks" {
  account_id = var.account_id
  name       = "eks-gitops-demo"
  config_src = "cloudflare"

  lifecycle {
    prevent_destroy = true # a new tunnel means a new token and new DNS targets: an outage
  }
}

resource "cloudflare_zero_trust_tunnel_cloudflared_config" "eks" {
  account_id = var.account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.eks.id
  config = {
    ingress = concat(
      flatten([for domain in var.routed_domains : [for host, cfg in local.hosts : concat(
        # A host limited to some paths first refuses dot segments (../, %2e%2e):
        # cloudflared matches the decoded path but forwards the raw one, and the
        # origin would resolve /realms/demo/../../admin to /admin.
        cfg.path == null ? [] : [{ hostname = "${host}.${domain}", path = "\\.\\.", service = "http_status:404" }],
        [{ hostname = "${host}.${domain}", path = cfg.path, service = cfg.service }],
      )]]),
      # Catch-all, required last: any other hostname (a zone's hosts before its
      # domain is routed), and auth. paths outside the demo realm and
      # /resources, get a 404 from cloudflared.
      [{ hostname = null, path = null, service = "http_status:404" }],
    )
  }

  # A gated host's Access applications know the host before the tunnel routes it.
  depends_on = [cloudflare_zero_trust_access_application.this, cloudflare_zero_trust_access_application.public]
}

data "cloudflare_zero_trust_tunnel_cloudflared_token" "eks" {
  account_id = var.account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.eks.id
}

# The connector's only credential, delivered to cloudflared by external-secrets
# (its read grant is in infra/eks). Anyone holding it can serve this tunnel's
# traffic, which is why this root's state is denied to the pull-request role.
resource "aws_secretsmanager_secret" "tunnel_token" {
  name                    = "demo/cloudflare/tunnel-token"
  description             = "Cloudflare Tunnel token for cloudflared (plain string); written by infra/cloudflare"
  recovery_window_in_days = 0 # demo: allow immediate re-create after destroy
}

resource "aws_secretsmanager_secret_version" "tunnel_token" {
  secret_id     = aws_secretsmanager_secret.tunnel_token.id
  secret_string = data.cloudflare_zero_trust_tunnel_cloudflared_token.eks.token
}
