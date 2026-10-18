output "cluster_name" {
  value = module.eks.cluster_name
}

output "cluster_endpoint" {
  value = module.eks.cluster_endpoint
}

output "cluster_certificate_authority_data" {
  value = module.eks.cluster_certificate_authority_data
}

output "vpc_id" {
  value = module.vpc.vpc_id
}

output "rds_endpoint" {
  value = aws_db_instance.this.address
}

output "rds_master_secret_arn" {
  value = aws_db_instance.this.master_user_secret[0].secret_arn
}

output "region" {
  value = var.region
}

output "amp_workspace_id" {
  value = aws_prometheus_workspace.this.id
}

output "amp_workspace_arn" {
  value = aws_prometheus_workspace.this.arn
}

output "amp_endpoint" {
  description = "Managed Prometheus query endpoint (remote write is <endpoint>/api/v1/remote_write)."
  value       = trimsuffix(aws_prometheus_workspace.this.prometheus_endpoint, "/")
}

output "apps_log_group" {
  value = aws_cloudwatch_log_group.apps.name
}

output "alerts_topic_arn" {
  value = aws_sns_topic.alerts.arn
}
