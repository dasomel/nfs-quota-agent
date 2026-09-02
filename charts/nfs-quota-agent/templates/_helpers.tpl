{{/*
Expand the name of the chart.
*/}}
{{- define "nfs-quota-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "nfs-quota-agent.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "nfs-quota-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "nfs-quota-agent.labels" -}}
helm.sh/chart: {{ include "nfs-quota-agent.chart" . }}
{{ include "nfs-quota-agent.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "nfs-quota-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nfs-quota-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "nfs-quota-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "nfs-quota-agent.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the image name. When .Values.image.digest is set, pin by digest
(repository@sha256:...) instead of the mutable tag -- the air-gap/
reproducible-release digest pinning #5 asks for. The tag is dropped
entirely rather than combined (not repository:tag@sha256:...): this
repo's own release tooling (hack/verify-release.py, release-manifest.json)
already reasons about the image purely as `<repository>@<digest>`, and a
digest makes the tag redundant for pulling, so rendering the same shape
here avoids two different "canonical" spellings of a pinned image existing
side by side. Kubernetes/containerd would resolve either form to the same
content since the digest always wins, but keeping one convention is less
surprising for anyone diffing rendered manifests against verify-release.py
output. digest is validated here (not in a values.schema.json, which this
chart does not have) so a malformed value fails render instead of being
silently passed to the container runtime as part of an invalid reference.
*/}}
{{- define "nfs-quota-agent.image" -}}
{{- if .Values.image.digest }}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" .Values.image.digest) }}
{{- fail (printf "image.digest %q is not a valid sha256 digest (expected sha256: followed by 64 lowercase hex characters)" .Values.image.digest) }}
{{- end }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- $tag := default .Chart.AppVersion .Values.image.tag }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}
{{- end }}
