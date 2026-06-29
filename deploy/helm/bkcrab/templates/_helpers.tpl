{{- define "bkcrab.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "bkcrab.labels" -}}
app.kubernetes.io/name: bkcrab
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "bkcrab.storageType" -}}
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
{{- define "bkcrab.dsn" -}}
{{- if .Values.externalDSN -}}
{{ .Values.externalDSN }}
{{- else if .Values.mysql.enabled -}}
bkcrab:{{ required "mysql.password is required when mysql.enabled=true" .Values.mysql.password }}@tcp({{ include "bkcrab.fullname" . }}-db:3306)/bkcrab?parseTime=true&loc=UTC&charset=utf8mb4
{{- else if .Values.postgres.enabled -}}
postgres://bkcrab:{{ required "postgres.password is required when postgres.enabled=true" .Values.postgres.password }}@{{ include "bkcrab.fullname" . }}-db:5432/bkcrab?sslmode=disable
{{- else -}}
{{- fail "Either externalDSN, mysql.enabled, or postgres.enabled must be set" -}}
{{- end -}}
{{- end -}}
