{{- define "g2k.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "g2k.fullname" -}}
{{- include "g2k.name" . }}-{{ .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

