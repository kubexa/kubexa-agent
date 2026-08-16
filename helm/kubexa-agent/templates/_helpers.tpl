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

{{/*
GOMEMLIMIT — the Go runtime's soft memory limit, in bytes.

Without it the runtime cannot see the container's memory limit at all: with the
default GOGC=100 it targets a heap of twice the live heap, whatever the cgroup
allows. That is not a theory here. A v0.6.0 agent under a 512Mi limit sat at a
dead-flat 432 MB RSS for 9.5 hours (2 x a ~216 MB live heap, 96% of the wall)
and was OOMKilled 8 times, each time ~20 seconds after start -- the window where
the initial informer sync, the disk-queue drain and the log streams all allocate
at once.

GOMEMLIMIT is SOFT: if the live heap genuinely exceeds it Go does not abort, it
runs the collector harder, and the runtime's own 50% CPU ceiling stops that from
becoming a death spiral. The failure mode it trades into is "slower", not
"killed".

Resolution order:
  goMemLimit: "off"    -> not set at all
  goMemLimit: "<any>"  -> used verbatim (Go accepts a byte count or 400MiB)
  goMemLimit: ""       -> goMemLimitPercent of resources.limits.memory
  no memory limit set  -> not set at all, since there is nothing to derive from

An unparseable limit FAILS the render. Guessing a byte count from a quantity we
did not understand would hand the runtime a target with no relation to the wall,
which is the exact bug this helper exists to remove.
*/}}
{{- define "kubexa-agent.goMemLimit" -}}
{{- $explicit := .Values.goMemLimit | default "" | toString -}}
{{- if eq $explicit "off" -}}
{{- else if $explicit -}}
{{- $explicit -}}
{{- else -}}
{{- $q := (((.Values.resources).limits).memory) | default "" | toString -}}
{{- if $q -}}
{{- $digits := regexFind "^[0-9]+" $q -}}
{{- $unit := trimPrefix $digits $q -}}
{{- $mult := index (dict "" 1 "k" 1000 "K" 1000 "M" 1000000 "G" 1000000000 "T" 1000000000000 "Ki" 1024 "Mi" 1048576 "Gi" 1073741824 "Ti" 1099511627776) $unit -}}
{{- if or (not $digits) (not $mult) -}}
{{- fail (printf "kubexa-agent: cannot parse resources.limits.memory %q into bytes for GOMEMLIMIT. Set goMemLimit to an explicit value (e.g. \"400MiB\") or to \"off\"." $q) -}}
{{- end -}}
{{- $pct := .Values.goMemLimitPercent | default 75 | int -}}
{{- printf "%d" (div (mul (mul (atoi $digits) $mult) $pct) 100) -}}
{{- end -}}
{{- end -}}
{{- end }}
