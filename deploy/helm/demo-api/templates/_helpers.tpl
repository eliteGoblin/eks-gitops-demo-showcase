{{- define "demo-api.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "demo-api.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "demo-api.labels" -}}
{{ include "demo-api.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* DATABASE_URL from the existing Secret when the Postgres store is on. */}}
{{- define "demo-api.databaseEnv" -}}
{{- if .Values.database.enabled }}
{{- if not .Values.database.existingSecret }}
{{- fail "database.existingSecret is required when database.enabled is true" }}
{{- end }}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret }}
      key: {{ .Values.database.urlKey }}
{{- end }}
{{- end }}

{{- define "demo-api.paymentsEnv" -}}
{{- if .Values.payments.enabled }}
{{- if not .Values.payments.existingSecret }}
{{- fail "payments.existingSecret is required when payments.enabled is true" }}
{{- end }}
- name: PAYMENTS_CLIENT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ .Values.payments.existingSecret }}
      key: {{ .Values.payments.secretKey }}
{{- end }}
{{- end }}

{{/* The partner API key: as an environment variable and/or as a mounted file. */}}
{{- define "demo-api.partnerEnv" -}}
{{- with .Values.partnerKey }}
{{- if .envSecret }}
- name: PARTNER_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .envSecret }}
      key: {{ .key }}
{{- end }}
{{- if .fileSecret }}
- name: PARTNER_KEY_FILE
  value: {{ printf "%s/%s" (trimSuffix "/" .mountPath) .key | quote }}
{{- end }}
{{- end }}
{{- end }}

{{/* Secrets whose rotation should roll the pods (Reloader). */}}
{{- define "demo-api.reloadSecrets" -}}
{{- $secrets := list }}
{{- if and .Values.database.enabled .Values.database.reloadOnSecretChange }}
{{- $secrets = append $secrets .Values.database.existingSecret }}
{{- end }}
{{- if and .Values.payments.enabled .Values.payments.reloadOnSecretChange }}
{{- $secrets = append $secrets .Values.payments.existingSecret }}
{{- end }}
{{- /* Only the env delivery needs a restart; the file one reloads in place. */}}
{{- with .Values.partnerKey.envSecret }}
{{- $secrets = append $secrets . }}
{{- end }}
{{- join "," $secrets }}
{{- end }}
