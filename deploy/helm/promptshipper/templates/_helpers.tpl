{{- define "promptshipper.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "promptshipper.fullname" -}}
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

{{/* <fullname>-<component>. Call with (dict "root" $ "component" "stt"). */}}
{{- define "promptshipper.componentName" -}}
{{- printf "%s-%s" (include "promptshipper.fullname" .root) .component | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "promptshipper.selectorLabels" -}}
app.kubernetes.io/name: {{ include "promptshipper.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "promptshipper.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "promptshipper.selectorLabels" . }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: promptshipper
{{- with .root.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* image reference for a component's values (dict "root" $ "c" .Values.stt) */}}
{{- define "promptshipper.image" -}}
{{- $tag := default .root.Chart.AppVersion .c.image.tag -}}
{{- if .c.image.digest -}}
{{- printf "%s@%s" .c.image.repository .c.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .c.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{- define "promptshipper.serviceName" -}}
{{- default (include "promptshipper.componentName" .) .c.service.name -}}
{{- end -}}

{{/* The Secret holding a component's token: existingSecret, or the chart's. */}}
{{- define "promptshipper.authSecretName" -}}
{{- if .c.auth.existingSecret -}}
{{- .c.auth.existingSecret -}}
{{- else if .c.auth.token -}}
{{- printf "%s-auth" (include "promptshipper.componentName" .) -}}
{{- else -}}
{{- fail (printf "%s: set %s.auth.existingSecret (recommended) or %s.auth.token — the service is never deployed without a bearer token" .component .component .component) -}}
{{- end -}}
{{- end -}}

{{/* The PVC a component mounts: existingClaim, or the chart's. Empty when persistence is off. */}}
{{- define "promptshipper.claimName" -}}
{{- if .c.persistence.enabled -}}
{{- default (printf "%s-data" (include "promptshipper.componentName" .)) .c.persistence.existingClaim -}}
{{- end -}}
{{- end -}}
