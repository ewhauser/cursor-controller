{{- define "cursor-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cursor-controller.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "cursor-controller.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "cursor-controller.labels" -}}
app.kubernetes.io/name: {{ include "cursor-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "cursor-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cursor-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
{{- end -}}

{{- define "cursor-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- .Values.serviceAccount.name | default (include "cursor-controller.fullname" .) -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "cursor-controller.secretName" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-api-key" (include "cursor-controller.fullname" . | trunc 55 | trimSuffix "-") -}}
{{- end -}}
{{- end -}}

{{- define "cursor-controller.templatesConfigMapName" -}}
{{- printf "%s-templates" (include "cursor-controller.fullname" . | trunc 53 | trimSuffix "-") -}}
{{- end -}}

{{- define "cursor-controller.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}

{{- define "cursor-controller.workerImage" -}}
{{- $repo := required "worker.image.repository is required (or set worker.podTemplate)" .Values.worker.image.repository -}}
{{- if .Values.worker.image.digest -}}
{{- printf "%s@%s" $repo .Values.worker.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (required "worker.image.tag is required" .Values.worker.image.tag | toString) -}}
{{- end -}}
{{- end -}}

{{- define "cursor-controller.workerDir" -}}
{{- if .Values.worker.workerDir -}}
{{- .Values.worker.workerDir -}}
{{- else if .Values.persistence.enabled -}}
{{- .Values.persistence.mountPath -}}
{{- else -}}
/workspace
{{- end -}}
{{- end -}}

{{- define "cursor-controller.managementPort" -}}
{{- $addr := .Values.worker.managementAddr | toString -}}
{{- if contains ":" $addr -}}{{- splitList ":" $addr | last -}}{{- else -}}{{- $addr -}}{{- end -}}
{{- end -}}

{{- define "cursor-controller.validate" -}}
{{- if not .Values.auth.existingSecret -}}
{{- $_ := required "Set auth.existingSecret or auth.apiKey (team service-account API key)." .Values.auth.apiKey -}}
{{- end -}}
{{- if and (gt (.Values.controller.warmIdle | int) 0) (not .Values.pools) -}}
{{- fail "controller.warmIdle > 0 requires at least one entry in pools." -}}
{{- end -}}
{{- if and .Values.otel.enabled (not .Values.otel.endpoint) -}}
{{- fail "otel.enabled requires otel.endpoint (or set OTEL_EXPORTER_OTLP_ENDPOINT via controller.extraEnv)." -}}
{{- end -}}
{{- if and .Values.persistence.enabled (not .Values.persistence.claimSpec) -}}
{{- fail "persistence.claimSpec is required when persistence.enabled=true." -}}
{{- end -}}
{{- end -}}

{{/*
Worker Pod manifest handed to the controller as --pod-template. The controller
sets name/namespace/labels, forces restartPolicy Never, injects CURSOR_* env
and the workspace volume, and adds CURSOR_API_KEY from the Secret.
*/}}
{{- define "cursor-controller.workerPod" -}}
{{- if .Values.worker.podTemplate -}}
{{ toYaml .Values.worker.podTemplate }}
{{- else -}}
apiVersion: v1
kind: Pod
metadata:
  labels:
    app.kubernetes.io/name: {{ include "cursor-controller.name" . }}
    app.kubernetes.io/instance: {{ .Release.Name }}
    {{- with .Values.worker.podLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  {{- with .Values.worker.podAnnotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  restartPolicy: Never
  automountServiceAccountToken: {{ .Values.worker.automountServiceAccountToken }}
  terminationGracePeriodSeconds: {{ .Values.worker.terminationGracePeriodSeconds }}
  {{- with .Values.worker.priorityClassName }}
  priorityClassName: {{ . | quote }}
  {{- end }}
  {{- with (default .Values.imagePullSecrets .Values.worker.imagePullSecrets) }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.worker.podSecurityContext }}
  securityContext:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  containers:
    - name: worker
      image: {{ include "cursor-controller.workerImage" . | quote }}
      imagePullPolicy: {{ .Values.worker.image.pullPolicy }}
      command:
        {{- toYaml .Values.worker.command | nindent 8 }}
      args:
        - worker
        - --pool
        - $(CURSOR_POOL)
        - --idle-release-timeout
        - {{ .Values.worker.idleReleaseTimeout | int | quote }}
        {{- $dir := include "cursor-controller.workerDir" . }}
        {{- if $dir }}
        - --worker-dir
        - {{ $dir | quote }}
        {{- end }}
        {{- if .Values.worker.managementAddr }}
        - --management-addr
        - {{ .Values.worker.managementAddr | quote }}
        {{- end }}
        {{- range .Values.worker.labels }}
        - --label
        - {{ . | quote }}
        {{- end }}
        {{- range .Values.worker.extraArgs }}
        - {{ . | quote }}
        {{- end }}
        - start
      {{- with .Values.worker.env }}
      env:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- if .Values.worker.managementAddr }}
      ports:
        - name: management
          containerPort: {{ include "cursor-controller.managementPort" . | int }}
          protocol: TCP
      {{- if .Values.worker.probes.enabled }}
      startupProbe:
        httpGet:
          path: {{ .Values.worker.probes.startup.path | quote }}
          port: management
        periodSeconds: {{ .Values.worker.probes.startup.periodSeconds }}
        failureThreshold: {{ .Values.worker.probes.startup.failureThreshold }}
      readinessProbe:
        httpGet:
          path: {{ .Values.worker.probes.readiness.path | quote }}
          port: management
        initialDelaySeconds: {{ .Values.worker.probes.readiness.initialDelaySeconds }}
        periodSeconds: {{ .Values.worker.probes.readiness.periodSeconds }}
      livenessProbe:
        httpGet:
          path: {{ .Values.worker.probes.liveness.path | quote }}
          port: management
        initialDelaySeconds: {{ .Values.worker.probes.liveness.initialDelaySeconds }}
        periodSeconds: {{ .Values.worker.probes.liveness.periodSeconds }}
      {{- end }}
      {{- end }}
      resources:
        {{- toYaml .Values.worker.resources | nindent 8 }}
      {{- with .Values.worker.securityContext }}
      securityContext:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.worker.extraVolumeMounts }}
      volumeMounts:
        {{- toYaml . | nindent 8 }}
      {{- end }}
  {{- with .Values.worker.extraVolumes }}
  volumes:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.worker.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.worker.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.worker.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.worker.topologySpreadConstraints }}
  topologySpreadConstraints:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}
{{- end -}}

{{- define "cursor-controller.validateSnapshot" -}}
{{- if .Values.persistence.snapshotSelector -}}
{{- if not .Values.persistence.enabled -}}
{{- fail "persistence.snapshotSelector requires persistence.enabled" -}}
{{- end -}}
{{- if or .Values.persistence.claimSpec.dataSource .Values.persistence.claimSpec.dataSourceRef -}}
{{- fail "persistence.snapshotSelector conflicts with claimSpec.dataSource/dataSourceRef" -}}
{{- end -}}
{{- end -}}
{{- end -}}
