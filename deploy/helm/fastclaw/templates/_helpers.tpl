{{- define "bkclaw.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "bkclaw.labels" -}}
app.kubernetes.io/name: bkclaw
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "bkclaw.storageType" -}}
{{- if and .Values.mysql.enabled .Values.postgres.enabled -}}
{{- fail "mysql.enabled and postgres.enabled cannot both be true" -}}
{{- else if .Values.externalDSN -}}
{{ required "externalDatabaseType is required when externalDSN is set" .Values.externalDatabaseType }}
{{- else if .Values.mysql.enabled -}}
mysql
{{- else if .Values.postgres.enabled -}}
postgres
{{- else -}}
{{- fail "Either externalDSN, mysql.enabled, or postgres.enabled must be set" -}}
{{- end -}}
{{- end -}}

{{- /* DSN：优先使用 externalDSN，否则使用选定的捆绑数据库。 */ -}}
{{- define "bkclaw.dsn" -}}
{{- if .Values.externalDSN -}}
{{ .Values.externalDSN }}
{{- else if .Values.mysql.enabled -}}
bkclaw:{{ required "mysql.password is required when mysql.enabled=true" .Values.mysql.password }}@tcp({{ include "bkclaw.fullname" . }}-db:3306)/bkclaw?parseTime=true&loc=UTC&charset=utf8mb4
{{- else if .Values.postgres.enabled -}}
postgres://bkclaw:{{ required "postgres.password is required when postgres.enabled=true" .Values.postgres.password }}@{{ include "bkclaw.fullname" . }}-db:5432/bkclaw?sslmode=disable
{{- else -}}
{{- fail "Either externalDSN, mysql.enabled, or postgres.enabled must be set" -}}
{{- end -}}
{{- end -}}
