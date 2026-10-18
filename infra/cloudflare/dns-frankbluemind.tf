# What frankbluemind.com served at GoDaddy (GET /v1/domains/frankbluemind.com/records
# and dig at ns01/ns02.domaincontrol.com, 2026-10-18), recreated as it was: DNS-only,
# GoDaddy's TTLs, so that the name-server switch changes no answer. The apex stays
# GoDaddy's Website Builder page (the API lists that A record as "WebsiteBuilder Site";
# the addresses are what GoDaddy's name servers answer) and mail keeps forwarding
# through ImprovMX (MX, SPF, DMARC). Not copied: the apex NS records, which name the
# old provider's servers (Cloudflare serves its own). _domainconnect is GoDaddy's
# Domain Connect discovery record, kept as found; it does nothing once GoDaddy no
# longer serves the zone. The content of a TXT record is sent quoted, which is how
# the API stores and returns it (an unquoted value would differ on every plan).
locals {
  godaddy_records = {
    "A frankbluemind.com 76.223.105.230"     = { name = "frankbluemind.com", type = "A", content = "76.223.105.230", priority = null }
    "A frankbluemind.com 13.248.243.5"       = { name = "frankbluemind.com", type = "A", content = "13.248.243.5", priority = null }
    "CNAME www.frankbluemind.com"            = { name = "www.frankbluemind.com", type = "CNAME", content = "frankbluemind.com", priority = null }
    "CNAME _domainconnect.frankbluemind.com" = { name = "_domainconnect.frankbluemind.com", type = "CNAME", content = "_domainconnect.gd.domaincontrol.com", priority = null }
    "MX frankbluemind.com 10"                = { name = "frankbluemind.com", type = "MX", content = "mx1.improvmx.com", priority = 10 }
    "MX frankbluemind.com 20"                = { name = "frankbluemind.com", type = "MX", content = "mx2.improvmx.com", priority = 20 }
    "TXT frankbluemind.com spf"              = { name = "frankbluemind.com", type = "TXT", content = "\"v=spf1 include:spf.improvmx.com ~all\"", priority = null }
    "TXT _dmarc.frankbluemind.com"           = { name = "_dmarc.frankbluemind.com", type = "TXT", content = "\"v=DMARC1; p=quarantine; adkim=r; aspf=r; rua=mailto:dmarc_rua@onsecureserver.net;\"", priority = null }
  }
}

resource "cloudflare_dns_record" "godaddy" {
  for_each = local.godaddy_records
  zone_id  = cloudflare_zone.this["frankbluemind.com"].id
  name     = each.value.name
  type     = each.value.type
  content  = each.value.content
  priority = each.value.priority
  ttl      = 3600
  proxied  = false
  comment  = "infra/cloudflare: copied from GoDaddy, 2026-10-18"
}
