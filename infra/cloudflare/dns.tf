locals {
  # The four public hosts: the in-cluster Service the tunnel sends each one to
  # (checked with kubectl on 2026-10-17), the paths it may reach, whether
  # Cloudflare Access gates it, and the paths Access lets through anyway.
  # auth. routes only the demo realm and Keycloak's static resources, so the
  # admin console, its API and the master realm stay unreachable. grafana.'s
  # public paths serve its externally shared dashboards (page, API, static
  # assets: routes of Grafana 12.3.1).
  hosts = {
    app     = { service = "http://demo-api.demo.svc.cluster.local:8082", path = null, access = false, public_paths = [] }
    auth    = { service = "http://keycloak.keycloak.svc.cluster.local:8080", path = "^/(realms/demo|resources)(/|$)", access = false, public_paths = [] }
    argocd  = { service = "http://argocd-server.argocd.svc.cluster.local:80", path = null, access = true, public_paths = [] }
    grafana = { service = "http://grafana.observability.svc.cluster.local:80", path = null, access = true, public_paths = ["/public-dashboards/*", "/api/public/*", "/public/*"] }
  }
  # Every zone carries the four hosts ("app.eksgitops.click" => zone and host),
  # so a zone is ready to serve the demo before it does.
  host_records = {
    for pair in setproduct(sort(tolist(var.zones)), keys(local.hosts)) :
    "${pair[1]}.${pair[0]}" => { zone = pair[0], host = pair[1] }
  }
  tunnel_cname = "${cloudflare_zero_trust_tunnel_cloudflared.eks.id}.cfargotunnel.com"
}

# Every host, in every zone, is a proxied CNAME to the tunnel, the only
# ingress: Cloudflare answers with its own anycast addresses, and a proxied
# record's TTL must be 1 ("automatic"). The tunnel routes a host only while its
# domain is in var.routed_domains; any other name reaches cloudflared's
# catch-all 404 (tunnel.tf), never a Service.
resource "cloudflare_dns_record" "host" {
  for_each = local.host_records
  zone_id  = cloudflare_zone.this[each.value.zone].id
  name     = each.key
  type     = "CNAME"
  content  = local.tunnel_cname
  proxied  = true
  ttl      = 1
  comment  = "infra/cloudflare: proxied to the tunnel"

  # A gated host's Access applications exist before its record does, so the
  # host is never reachable without Access.
  depends_on = [cloudflare_zero_trust_access_application.this, cloudflare_zero_trust_access_application.public]
}

# The records were keyed by host while the root served one zone (2026-10-19).
moved {
  from = cloudflare_dns_record.host["app"]
  to   = cloudflare_dns_record.host["app.eksgitops.click"]
}

moved {
  from = cloudflare_dns_record.host["auth"]
  to   = cloudflare_dns_record.host["auth.eksgitops.click"]
}

moved {
  from = cloudflare_dns_record.host["argocd"]
  to   = cloudflare_dns_record.host["argocd.eksgitops.click"]
}

moved {
  from = cloudflare_dns_record.host["grafana"]
  to   = cloudflare_dns_record.host["grafana.eksgitops.click"]
}
