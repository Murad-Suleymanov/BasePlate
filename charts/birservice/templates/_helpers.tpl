{{/*
Render the spec body of a BirService CR from a values map. Used by birservice.yaml
for both single-instance (values root) and multi-instance (per-instance map) cases.

Input: a dict with the per-workload fields (image, repo, hpa, resources, traffic, …).
Output: YAML for `spec:` — without the `spec:` key itself; the caller controls indent.
*/}}
{{- define "birservice.spec" -}}
{{- $v := . -}}
{{- if $v.image }}
image: {{ $v.image | quote }}
{{- end }}
{{- if $v.repo }}
repo: {{ $v.repo | quote }}
{{- end }}
{{- if $v.tag }}
tag: {{ $v.tag | quote }}
{{- end }}
{{- if $v.imageTag }}
imageTag: {{ $v.imageTag | quote }}
{{- end }}
{{- if $v.dockerfile }}
dockerfile: {{ $v.dockerfile | quote }}
{{- end }}
{{- $hpa := $v.hpa | default dict }}
{{- $min := (get $hpa "minReplicas") | default 0 | int }}
{{- $max := (get $hpa "maxReplicas") | default 0 | int }}
{{- $replicasSet := or (kindIs "float64" $v.replicas) (kindIs "int" $v.replicas) }}
{{- $useHPA := and (gt $min 0) (gt $max 0) (not $replicasSet) }}
{{- $replicas := $v.replicas | default 1 | int }}
{{- if not $useHPA }}
replicas: {{ $replicas }}
{{- end }}
{{- if $useHPA }}
hpa:
  minReplicas: {{ $min }}
  maxReplicas: {{ $max }}
{{- end }}
{{- $resources := $v.resources | default dict }}
{{- $requests := (get $resources "requests") | default dict }}
{{- $limits := (get $resources "limits") | default dict }}
{{- $reqMem := ((get $requests "memory") | default "" | toString | trim) }}
{{- $reqCPU := ((get $requests "cpu") | default "" | toString | trim) }}
{{- $limMem := ((get $limits "memory") | default "" | toString | trim) }}
{{- $limCPU := ((get $limits "cpu") | default "" | toString | trim) }}
{{- if or $reqMem $reqCPU $limMem $limCPU }}
resources:
{{- if or $reqMem $reqCPU }}
  requests:
{{- if $reqMem }}
    memory: {{ $reqMem | quote }}
{{- end }}
{{- if $reqCPU }}
    cpu: {{ $reqCPU | quote }}
{{- end }}
{{- end }}
{{- if or $limMem $limCPU }}
  limits:
{{- if $limMem }}
    memory: {{ $limMem | quote }}
{{- end }}
{{- if $limCPU }}
    cpu: {{ $limCPU | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- if $v.port }}
port: {{ $v.port }}
{{- end }}
{{- if $v.containerPort }}
containerPort: {{ $v.containerPort }}
{{- end }}
{{- if $v.hostname }}
hostname: {{ $v.hostname | quote }}
{{- end }}
{{- if ne (default true $v.expose) true }}
expose: false
{{- end }}
{{- if or (and (eq (kindOf $v.metrics) "bool") $v.metrics) (and (eq (kindOf $v.metrics) "map") (ne false $v.metrics.enabled)) }}
metrics:
  enabled: true
{{- if eq (kindOf $v.metrics) "map" }}
  path: {{ default "/metrics" $v.metrics.path | quote }}
{{- else }}
  path: "/metrics"
{{- end }}
{{- end }}
{{- if $v.traffic }}
traffic:
{{ toYaml $v.traffic | indent 2 }}
{{- end }}
{{- if $v.canary }}
canary:
{{ toYaml $v.canary | indent 2 }}
{{- end }}
{{- if $v.readinessProbe }}
readinessProbe:
{{ toYaml $v.readinessProbe | indent 2 }}
{{- end }}
{{- if $v.livenessProbe }}
livenessProbe:
{{ toYaml $v.livenessProbe | indent 2 }}
{{- end }}
{{- if hasKey $v "singleton" }}
singleton: {{ $v.singleton }}
{{- end }}
{{- if hasKey $v "maxDown" }}
maxDown: {{ $v.maxDown }}
{{- end }}
{{- if $v.shutdown }}
shutdown:
{{ toYaml $v.shutdown | indent 2 }}
{{- end }}
{{- if $v.route }}
route:
{{ toYaml $v.route | indent 2 }}
{{- end }}
{{- end -}}

{{/*
Detect whether values are multi-instance shape.
Returns "true" if any top-level key is a map AND is not a known operational or
service-level field. Defaults in values.yaml always provide hpa, resources, etc.,
so presence of a known key cannot signal single-instance — only the presence of
an UNKNOWN map key (the instance name) signals multi-instance.

Uses a dict for state because Go template `range` creates a new variable scope —
direct `$var = ...` assignments inside `range` don't reliably persist outside.
*/}}
{{- define "birservice.isMultiInstance" -}}
{{- $knownKeys := list "name" "owner" "image" "repo" "tag" "imageTag" "dockerfile" "port" "containerPort" "replicas" "hpa" "resources" "hostname" "expose" "metrics" "traffic" "readinessProbe" "livenessProbe" "singleton" "maxDown" "shutdown" "canary" "injectPipeline" -}}
{{- $state := dict "hasInstance" false -}}
{{- range $k, $val := . -}}
  {{- if and (not (has $k $knownKeys)) (kindIs "map" $val) -}}
    {{- $_ := set $state "hasInstance" true -}}
  {{- end -}}
{{- end -}}
{{- if $state.hasInstance -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{/*
Common labels for the BirService CR. Owner is propagated as both label (sanitized) and annotation (raw).
*/}}
{{- define "birservice.labels" -}}
app.kubernetes.io/managed-by: "argocd"
deploy.easydeploy.io/tenant: {{ .Release.Namespace | quote }}
{{- end -}}
