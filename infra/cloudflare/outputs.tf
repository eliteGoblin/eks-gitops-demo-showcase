output "zone_ids" {
  value = { for name, zone in cloudflare_zone.this : name => zone.id }
}

output "nameservers" {
  description = "The name servers Cloudflare assigned to each zone: set them at that zone's registrar for its switch."
  value       = { for name, zone in cloudflare_zone.this : name => zone.name_servers }
}

output "tunnel_id" {
  value = cloudflare_zero_trust_tunnel_cloudflared.eks.id
}

output "tunnel_cname" {
  description = "What every host's record points at."
  value       = local.tunnel_cname
}

output "access_callback_url" {
  description = "Redirect URI that the Keycloak client cloudflare-access must allow."
  value       = "https://${var.access_team_domain}/cdn-cgi/access/callback"
}
