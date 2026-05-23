{{/*
Expand the name of the chart.
*/}}
{{- define "nvelox-ingress-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name. Truncated to 63 chars to fit DNS-1123.
*/}}
{{- define "nvelox-ingress-controller.fullname" -}}
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
Common labels applied to every object.
*/}}
{{- define "nvelox-ingress-controller.labels" -}}
app.kubernetes.io/name: {{ include "nvelox-ingress-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: nvelox
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels (subset of common labels — these must be immutable on
a Deployment, so version/chart get excluded).
*/}}
{{- define "nvelox-ingress-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nvelox-ingress-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name — always derived from fullname so RBAC bindings
stay tied to it.
*/}}
{{- define "nvelox-ingress-controller.serviceAccountName" -}}
{{- include "nvelox-ingress-controller.fullname" . }}
{{- end }}

{{/*
Controller image — tag falls back to .Chart.AppVersion when unset so
chart upgrades pull the matched controller binary by default.
*/}}
{{- define "nvelox-ingress-controller.controllerImage" -}}
{{- $tag := default .Chart.AppVersion .Values.controller.image.tag -}}
{{- printf "%s:%s" .Values.controller.image.repository $tag }}
{{- end }}

{{/*
nvelox sidecar image.
*/}}
{{- define "nvelox-ingress-controller.nveloxImage" -}}
{{- printf "%s:%s" .Values.nvelox.image.repository (default "latest" .Values.nvelox.image.tag) }}
{{- end }}
