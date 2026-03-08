{{/*
Expand the name of the chart.
*/}}
{{- define "replic2.name" -}}
{{- .Chart.Name }}
{{- end }}

{{/*
Create a fully qualified app name using the release name.
*/}}
{{- define "replic2.fullname" -}}
{{- .Release.Name }}
{{- end }}

{{/*
Common labels applied to every resource.
*/}}
{{- define "replic2.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "replic2.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels used by Deployment and Service.
*/}}
{{- define "replic2.selectorLabels" -}}
app.kubernetes.io/name: {{ include "replic2.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The fully-qualified container image reference.
*/}}
{{- define "replic2.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}
