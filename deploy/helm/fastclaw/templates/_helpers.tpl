{{- define "fastclaw.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fastclaw.labels" -}}
app.kubernetes.io/name: fastclaw
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- /* DSN: prefer externalDSN, else fall back to bundled postgres. */ -}}
{{- define "fastclaw.dsn" -}}
{{- if .Values.externalDSN -}}
{{ .Values.externalDSN }}
{{- else if .Values.postgres.enabled -}}
postgres://fastclaw:{{ required "postgres.password is required when postgres.enabled=true" .Values.postgres.password }}@{{ include "fastclaw.fullname" . }}-db:5432/fastclaw?sslmode=disable
{{- else -}}
{{- fail "Either externalDSN or postgres.enabled must be set" -}}
{{- end -}}
{{- end -}}
