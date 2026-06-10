{{/*
Expand the name of the chart.
*/}}
{{- define "kubexa-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "kubexa-agent.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart and app labels.
*/}}
{{- define "kubexa-agent.labels" -}}
helm.sh/chart: {{ include "kubexa-agent.chart" . }}
{{ include "kubexa-agent.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "kubexa-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "kubexa-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kubexa-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "kubexa-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kubexa-agent.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Secret holding tenant token.
*/}}
{{- define "kubexa-agent.secretName" -}}
{{- if .Values.secret.existingSecret }}
{{- .Values.secret.existingSecret }}
{{- else }}
{{- include "kubexa-agent.fullname" . }}
{{- end }}
{{- end }}

{{/*
Container image reference.
*/}}
{{- define "kubexa-agent.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
Resolved gateway address (host:port).
*/}}
{{- define "kubexa-agent.gatewayAddress" -}}
{{- if .Values.gateway.address }}
{{- .Values.gateway.address }}
{{- else if .Values.gateway.host }}
{{- printf "%s:%v" .Values.gateway.host (.Values.gateway.port | toString) }}
{{- else }}
{{- fail "gateway.address or gateway.host must be set" }}
{{- end }}
{{- end }}

{{/*
ConfigMap name.
*/}}
{{- define "kubexa-agent.configMapName" -}}
{{- printf "%s-config" (include "kubexa-agent.fullname" .) }}
{{- end }}

{{/*
PVC name when persistence is enabled.
*/}}
{{- define "kubexa-agent.pvcName" -}}
{{- printf "%s-data" (include "kubexa-agent.fullname" .) }}
{{- end }}
