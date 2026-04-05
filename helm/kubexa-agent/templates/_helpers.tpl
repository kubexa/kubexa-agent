{{/*
Application Name
*/}}
{{- define "kubexa-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Namespace: .Values.namespace doluysa onu kullan, yoksa .Release.Namespace
*/}}
{{- define "kubexa-agent.namespace" -}}
{{- .Values.namespace | default .Release.Namespace }}
{{- end }}

{{/*

fullname: release-name + chart-name
example: my-release-kubexa-agent
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
Chart label: chart-name + chart-version
*/}}
{{- define "kubexa-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels - using in all resources
*/}}
{{- define "kubexa-agent.labels" -}}
helm.sh/chart: {{ include "kubexa-agent.chart" . }}
{{ include "kubexa-agent.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels - using deployment and service selector
*/}}
{{- define "kubexa-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kubexa-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name
*/}}
{{- define "kubexa-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kubexa-agent.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}