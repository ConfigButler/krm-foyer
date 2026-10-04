{{- define "krm-foyer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The release name, unless it already contains the chart's name. Truncated to 63
characters, the limit of a DNS label.
*/}}
{{- define "krm-foyer.fullname" -}}
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

{{- define "krm-foyer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "krm-foyer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "krm-foyer.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "krm-foyer.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "krm-foyer.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "krm-foyer.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* The shared-watch identity: never the pod's own account. */}}
{{- define "krm-foyer.sharedAccountName" -}}
{{- $name := default (printf "%s-shared" (include "krm-foyer.fullname" .)) .Values.sharedWatches.serviceAccount.name }}
{{- if eq $name (include "krm-foyer.serviceAccountName" .) }}
{{- fail "sharedWatches.serviceAccount.name must not be the pod's own service account: krm-foyer never uses that account, and a projected token is the pod's own" }}
{{- end }}
{{- $name }}
{{- end }}

{{/*
A flag for each value set in a map, in the order of the pairs given: "key" "flag".
Integers are printed as integers, which Helm would otherwise hand over as floats
(1.6777216e+07) for a number from a values file.
*/}}
{{- define "krm-foyer.flags" -}}
{{- $values := index . 0 }}
{{- range $pair := index . 1 }}
{{- $key := index $pair 0 }}
{{- if hasKey $values $key }}
{{- $v := index $values $key }}
{{- if kindIs "float64" $v }}
{{- if eq $v (floor $v) }}
{{- $v = int64 $v }}
{{- end }}
{{- end }}
- {{ printf "-%s=%v" (index $pair 1) $v | quote }}
{{- end }}
{{- end }}
{{- end }}
