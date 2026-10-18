# Links to the previous hosts keep working: one Page Rule per zone in
# var.redirect_domains sends every request for *.<zone>/* to the same host and
# path under var.domain with a 301 (the Free plan allows three Page Rules per
# zone; one wildcard rule covers the four hosts, $1 the host label and $2 the
# path with its query string). The old hosts stay proxied CNAMEs to the tunnel
# (dns.tf), which is what lets the edge answer for them; they are no longer
# routed (var.routed_domains), so nothing would reach a Service even without
# the rule. OIDC clients configured with the old issuer are not helped by the
# redirect (the discovery document names the new issuer): they must be
# re-pointed. None exist outside this repository.
resource "cloudflare_page_rule" "redirect" {
  for_each = toset(var.redirect_domains)
  zone_id  = cloudflare_zone.this[each.key].id
  target   = "*.${each.key}/*"
  priority = 1
  status   = "active"
  actions = {
    forwarding_url = {
      url         = "https://$1.${var.domain}/$2"
      status_code = 301
    }
  }
}
