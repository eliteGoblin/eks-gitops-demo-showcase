# Observability platform: AWS-managed backends (Managed Prometheus, X-Ray,
# CloudWatch Logs, SNS) plus the IAM that lets the in-cluster collector,
# Fluent Bit and Grafana use them through Pod Identity. The Kubernetes side
# (ADOT collector, kube-state-metrics, Fluent Bit, Grafana) is installed by
# Argo CD from the gitops repo; it learns these resources from the bridge
# annotations (see infra/argocd).

resource "aws_prometheus_workspace" "this" {
  alias = var.name
}

resource "aws_cloudwatch_log_group" "apps" {
  name              = "/${var.name}/apps"
  retention_in_days = 7
}

resource "aws_sns_topic" "alerts" {
  name = "${var.name}-alerts"

  # First change applied by CI on merge after TF_AUTO_APPLY was turned on
  # (harmless in-place tag update; kept as a marker of who manages the topic).
  tags = { Pipeline = "tf-apply" }
}

# Email subscriptions stay "pending confirmation" until the recipient clicks
# the link AWS sends; Terraform cannot confirm them.
resource "aws_sns_topic_subscription" "alerts_email" {
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "email"
  endpoint  = var.alerts_email
}

# Secret containers for Grafana. Values are written by scripts/secrets-seed.sh
# (`make eks-up` runs it right after this root is applied; `make secrets-seed`
# runs it alone), never by Terraform, so nothing secret lands in state.
resource "aws_secretsmanager_secret" "grafana" {
  for_each = {
    admin = "Grafana built-in admin (break-glass and API provisioning): username, password"
    oidc  = "Grafana OIDC client secret shared by Keycloak and Grafana: clientSecret"
  }
  name                    = "demo/grafana/${each.key}"
  description             = each.value
  recovery_window_in_days = 0 # demo: allow immediate re-create after destroy
}

# external-secrets may read the Grafana secrets too (its base policy is scoped
# to the bootstrap-layer secrets and the RDS master secret).
resource "aws_iam_role_policy" "external_secrets_grafana" {
  name = "grafana-secrets"
  role = module.external_secrets_identity.iam_role_name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "ReadGrafanaSecrets"
      Effect   = "Allow"
      Action   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
      Resource = [for s in aws_secretsmanager_secret.grafana : s.arn]
    }]
  })
}

locals {
  xray_write_actions = [
    "xray:PutTraceSegments", "xray:PutTelemetryRecords",
    "xray:GetSamplingRules", "xray:GetSamplingTargets", "xray:GetSamplingStatisticSummaries",
  ]
  xray_read_actions = [
    "xray:BatchGetTraces", "xray:GetTraceSummaries", "xray:GetServiceGraph", "xray:GetTraceGraph",
    "xray:GetGroups", "xray:GetGroup", "xray:GetTimeSeriesServiceStatistics",
    "xray:GetInsightSummaries", "xray:GetInsight", "xray:GetInsightEvents", "xray:GetInsightImpactGraph",
  ]
  logs_write_actions = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
  logs_read_actions = [
    "logs:StartQuery", "logs:StopQuery", "logs:GetQueryResults", "logs:GetLogEvents",
    "logs:FilterLogEvents", "logs:GetLogGroupFields", "logs:GetLogRecord", "logs:DescribeLogStreams",
  ]
}

# ADOT collector: traces → X-Ray, metrics → Managed Prometheus (remote write).
module "adot_collector_identity" {
  source  = "terraform-aws-modules/eks-pod-identity/aws"
  version = "~> 2.9"

  name                 = "${var.name}-adot-collector"
  attach_custom_policy = true
  policy_statements = [
    {
      sid       = "RemoteWrite"
      actions   = ["aps:RemoteWrite", "aps:GetSeries", "aps:GetLabels", "aps:GetMetricMetadata"]
      resources = [aws_prometheus_workspace.this.arn]
    },
    {
      sid       = "XRayWrite"
      actions   = local.xray_write_actions
      resources = ["*"]
    },
    {
      sid       = "AppLogsWrite"
      actions   = local.logs_write_actions
      resources = ["${aws_cloudwatch_log_group.apps.arn}:*"]
    },
    {
      sid       = "DescribeLogGroups"
      actions   = ["logs:DescribeLogGroups"]
      resources = ["*"]
    },
  ]
  associations = {
    main = {
      cluster_name    = module.eks.cluster_name
      namespace       = "observability"
      service_account = "adot-collector"
    }
  }
}

# Fluent Bit: container logs → the apps log group only.
module "fluent_bit_identity" {
  source  = "terraform-aws-modules/eks-pod-identity/aws"
  version = "~> 2.9"

  name                 = "${var.name}-fluent-bit"
  attach_custom_policy = true
  policy_statements = [
    {
      sid       = "AppLogsWrite"
      actions   = local.logs_write_actions
      resources = ["${aws_cloudwatch_log_group.apps.arn}:*"]
    },
    {
      sid       = "DescribeLogGroups"
      actions   = ["logs:DescribeLogGroups"]
      resources = ["*"]
    },
  ]
  associations = {
    main = {
      cluster_name    = module.eks.cluster_name
      namespace       = "observability"
      service_account = "fluent-bit"
    }
  }
}

# Grafana: read-only on the three backends, plus publish to the alert topic.
module "grafana_identity" {
  source  = "terraform-aws-modules/eks-pod-identity/aws"
  version = "~> 2.9"

  name                 = "${var.name}-grafana"
  attach_custom_policy = true
  policy_statements = [
    {
      sid       = "QueryPrometheus"
      actions   = ["aps:QueryMetrics", "aps:GetSeries", "aps:GetLabels", "aps:GetMetricMetadata"]
      resources = [aws_prometheus_workspace.this.arn]
    },
    {
      sid       = "XRayRead"
      actions   = local.xray_read_actions
      resources = ["*"]
    },
    {
      sid       = "AppLogsRead"
      actions   = local.logs_read_actions
      resources = [aws_cloudwatch_log_group.apps.arn, "${aws_cloudwatch_log_group.apps.arn}:*"]
    },
    {
      sid       = "LogsAndMetricsDiscovery"
      actions   = ["logs:DescribeLogGroups", "logs:GetQueryResults", "logs:StopQuery", "cloudwatch:GetMetricData", "cloudwatch:ListMetrics", "cloudwatch:GetMetricStatistics", "cloudwatch:DescribeAlarms", "ec2:DescribeRegions", "tag:GetResources"]
      resources = ["*"]
    },
    {
      sid       = "PublishAlerts"
      actions   = ["sns:Publish"]
      resources = [aws_sns_topic.alerts.arn]
    },
  ]
  associations = {
    main = {
      cluster_name    = module.eks.cluster_name
      namespace       = "observability"
      service_account = "grafana"
    }
  }
}
