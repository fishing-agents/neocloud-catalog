{{/* SPDX-FileCopyrightText: 2026 Reza Jelveh */}}
{{/* SPDX-License-Identifier: Apache-2.0 */}}
{{- define "catalog-service.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "catalog-service.fullname" -}}
{{- default .Chart.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "catalog-service.labels" -}}
app.kubernetes.io/name: {{ include "catalog-service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | default .Chart.Version }}
{{- end -}}

{{- define "catalog-service.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}{{ with .Values.image.digest }}@{{ . }}{{ end }}
{{- end -}}

{{- define "catalog-service.dsnEnv" -}}
- name: NEOCLOUD_CATALOG_DATABASE_DSN
  valueFrom:
    secretKeyRef:
      name: {{ required "database.dsnSecretName is required: the DSN comes from a Secret" .Values.database.dsnSecretName }}
      key: {{ .Values.database.dsnSecretKey }}
{{- end -}}

{{/*
The HF token Secret; empty in an operator deployment (no huggingface importer).
*/}}
{{- define "catalog-service.hfTokenSecret" -}}
{{- if not (.Values.config.upstream | default dict).ref }}{{ .Values.huggingface.tokenSecretName }}{{ end -}}
{{- end -}}

{{- define "catalog-service.securityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop: ["ALL"]
{{- end -}}

{{- define "catalog-service.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{/*
config.toml: .Values.config plus the keys the chart owns (listen addresses,
data dir, mounted file paths).
*/}}
{{- define "catalog-service.config" -}}
{{- $cfg := deepCopy (.Values.config | default dict) -}}
{{- $_ := set $cfg "server" (merge (dict "addr" (printf "0.0.0.0:%v" .Values.service.port)) ($cfg.server | default dict)) -}}
{{- $_ := set $cfg "telemetry" (merge (dict "metrics_addr" (printf "0.0.0.0:%v" .Values.service.metricsPort)) ($cfg.telemetry | default dict)) -}}
{{- $_ := set $cfg "bootstrap" (merge (dict "data_dir" "/data") ($cfg.bootstrap | default dict)) -}}
{{- if include "catalog-service.hfTokenSecret" . -}}
{{- $_ := set $cfg "huggingface" (merge (dict "token_file" "/var/run/secrets/catalog-service/huggingface/token") ($cfg.huggingface | default dict)) -}}
{{- end -}}
{{- $release := $cfg.release | default dict -}}
{{- if $release.ref -}}
{{- $_ := set $cfg "release" (merge (dict "key" "/var/run/secrets/catalog-service/signing/cosign.key") $release) -}}
{{- end -}}
{{- $trust := dict -}}
{{- with .Values.upstreamTrust.publicKey }}{{ $_ := set $trust "public_key" "/etc/catalog-service/upstream-public-key.pem" }}{{ end -}}
{{- with .Values.upstreamTrust.overlayPublicKey }}{{ $_ := set $trust "overlay_public_key" "/etc/catalog-service/upstream-overlay-public-key.pem" }}{{ end -}}
{{- with .Values.upstreamTrust.trustedRoot }}{{ $_ := set $trust "trusted_root" "/etc/catalog-service/upstream-trusted-root.json" }}{{ end -}}
{{- if $trust -}}
{{- $_ := set $cfg "upstream" (merge $trust ($cfg.upstream | default dict)) -}}
{{- end -}}
{{- toToml $cfg -}}
{{- end -}}
