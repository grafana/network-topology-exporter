{{/*
Expand the name of the chart.
*/}}
{{- define "topology-exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "topology-exporter.fullname" -}}
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
Create chart label.
*/}}
{{- define "topology-exporter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "topology-exporter.labels" -}}
helm.sh/chart: {{ include "topology-exporter.chart" . }}
{{ include "topology-exporter.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "topology-exporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "topology-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "topology-exporter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "topology-exporter.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Image reference. config.federation.role: hub runs a separate image built
from cmd/topology-hub (Dockerfile.hub, docs/proposals/core-hub-split.md §3);
every other role uses the main topology-exporter image. Same role check
NOTES.txt already uses to decide whether to print hub-specific guidance.
*/}}
{{- define "topology-exporter.image" -}}
{{- $repo := .Values.image.repository }}
{{- if eq ((.Values.config).federation | default dict).role "hub" }}
{{- $repo = .Values.image.hubRepository }}
{{- end }}
{{- printf "%s:%s" $repo (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
