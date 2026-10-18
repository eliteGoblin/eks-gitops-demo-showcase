# Full setup (the only one the Free plan offers): Cloudflare becomes
# authoritative for a zone when the registrar's name servers point at it. Until
# then the zone is "pending": Cloudflare answers only on its own name servers,
# and it deletes a zone that stays pending for 28 days. New zones start on the
# Free plan, and the API scans for existing records only when asked (the v5
# provider has no such option), so a zone holds exactly the records in this root.
resource "cloudflare_zone" "this" {
  for_each = var.zones
  account  = { id = var.account_id }
  name     = each.key
  type     = "full"

  # Pull requests never plan this root and merges apply at once, so a change
  # that would replace a zone (after its switch: the whole domain's DNS) must
  # fail the plan instead. Remove this only for a deliberate teardown.
  lifecycle {
    prevent_destroy = true
  }
}

# The root served one zone until frankbluemind.com joined (2026-10-19).
moved {
  from = cloudflare_zone.this
  to   = cloudflare_zone.this["eksgitops.click"]
}

locals {
  zone_settings = {
    ssl              = "strict" # Full (strict): the edge verifies an origin's certificate (tunnel routes don't use it)
    min_tls_version  = "1.2"
    always_use_https = "on" # proxied hosts redirect http:// to https:// at the edge; DNS-only records are not touched
  }
  zone_setting_pairs = {
    for pair in setproduct(sort(tolist(var.zones)), keys(local.zone_settings)) :
    "${pair[0]}/${pair[1]}" => { zone = pair[0], setting = pair[1] }
  }
}

resource "cloudflare_zone_setting" "this" {
  for_each   = local.zone_setting_pairs
  zone_id    = cloudflare_zone.this[each.value.zone].id
  setting_id = each.value.setting
  value      = local.zone_settings[each.value.setting]
}

moved {
  from = cloudflare_zone_setting.this["ssl"]
  to   = cloudflare_zone_setting.this["eksgitops.click/ssl"]
}

moved {
  from = cloudflare_zone_setting.this["min_tls_version"]
  to   = cloudflare_zone_setting.this["eksgitops.click/min_tls_version"]
}

moved {
  from = cloudflare_zone_setting.this["always_use_https"]
  to   = cloudflare_zone_setting.this["eksgitops.click/always_use_https"]
}
