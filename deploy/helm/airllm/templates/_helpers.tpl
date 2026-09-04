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

{{/*
WIF audience — the pool provider's resource name. Rendered in TWO places that must
agree (the projected token and the credential config), and a mismatch is only
rejected by STS at refresh time, long after rollout — so it is computed once here.
*/}}
{{- define "airllm.gcpWifAudience" -}}
{{- $wif := .Values.googleWorkloadIdentity -}}
{{- printf "//iam.googleapis.com/projects/%s/locations/global/workloadIdentityPools/%s/providers/%s" $wif.projectNumber $wif.poolId $wif.providerId -}}
{{- end -}}

{{/* Credential config the Google SDK reads (GOOGLE_APPLICATION_CREDENTIALS). */}}
{{- define "airllm.gcpWifCredentialFile" -}}
/var/run/secrets/gcp/creds/credential-config.json
{{- end -}}

{{/* Projected token the credential config points at. */}}
{{- define "airllm.gcpWifTokenFile" -}}
/var/run/secrets/gcp/tokens/token
{{- end -}}
