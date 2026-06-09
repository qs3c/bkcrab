{{- define "bkclaw.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "bkclaw.labels" -}}
app.kubernetes.io/name: bkclaw
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- /* DSN: prefer externalDSN, else fall back to bundled postgres. */ -}}
{{- define "bkclaw.dsn" -}}
{{- if .Values.externalDSN -}}
{{ .Values.externalDSN }}
{{- else if .Values.postgres.enabled -}}
postgres://bkclaw:{{ required "postgres.password is required when postgres.enabled=true" .Values.postgres.password }}@{{ include "bkclaw.fullname" . }}-db:5432/bkclaw?sslmode=disable
{{- else -}}
{{- fail "Either externalDSN or postgres.enabled must be set" -}}
{{- end -}}
{{- end -}}
