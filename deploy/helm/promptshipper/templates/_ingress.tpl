{{/* An optional Ingress in front of one component's Service. (dict "root" $ "component" .. "c" ..) */}}
{{- define "promptshipper.ingress" -}}
{{- if .c.ingress.enabled }}
{{- if not .c.ingress.hosts }}
{{- fail (printf "%s.ingress.enabled needs at least one entry in %s.ingress.hosts" .component .component) }}
{{- end }}
{{- $svc := include "promptshipper.serviceName" . }}
{{- $port := .c.service.port }}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "promptshipper.componentName" . }}
  labels:
    {{- include "promptshipper.labels" . | nindent 4 }}
  {{- with .c.ingress.annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  {{- with .c.ingress.className }}
  ingressClassName: {{ . }}
  {{- end }}
  {{- with .c.ingress.tls }}
  tls:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  rules:
    {{- range .c.ingress.hosts }}
    - host: {{ .host | quote }}
      http:
        paths:
          {{- range (default (list (dict "path" "/" "pathType" "Prefix")) .paths) }}
          - path: {{ .path }}
            pathType: {{ default "Prefix" .pathType }}
            backend:
              service:
                name: {{ $svc }}
                port:
                  number: {{ $port }}
          {{- end }}
    {{- end }}
{{- end }}
{{- end -}}
