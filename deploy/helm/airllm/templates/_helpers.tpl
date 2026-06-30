{{/* Chart name (overridable). */}}
{{- define "airllm.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully-qualified release name. */}}
{{- define "airllm.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Common labels. */}}
{{- define "airllm.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "airllm.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* Selector labels (name + instance; per-workload templates add app.kubernetes.io/component). */}}
{{- define "airllm.selectorLabels" -}}
app.kubernetes.io/name: {{ include "airllm.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Service account name. */}}
{{- define "airllm.serviceAccountName" -}}
{{- include "airllm.fullname" . -}}
{{- end -}}

{{/* In-cluster DLP sidecar Service name (used by the app's DLP model_url). */}}
{{- define "airllm.dlpBertServiceName" -}}
{{- printf "%s-dlp-bert" (include "airllm.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Image ref with tag defaulting to the chart appVersion. */}}
{{- define "airllm.image" -}}
{{- $tag := .image.tag | default .root.Chart.AppVersion -}}
{{- printf "%s:%s" .image.repository $tag -}}
{{- end -}}
