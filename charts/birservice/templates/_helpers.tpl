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
{{- $targetRPS := (get $hpa "targetRPS") | default 0 | int }}
{{- if gt $targetRPS 0 }}
  targetRPS: {{ $targetRPS }}
{{- end }}
{{- $scaleType := (get $hpa "scaleType") | default "" | toString | trim }}
{{- if $scaleType }}
  scaleType: {{ $scaleType | quote }}
{{- $target := (get $hpa "target") | default 0 | int }}
{{- if gt $target 0 }}
  target: {{ $target }}
{{- end }}
{{- end }}
{{- $window := (get $hpa "window") | default "" | toString | trim }}
{{- if $window }}
  window: {{ $window | quote }}
{{- end }}
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
{{- $pool := $v.pool | default $v.nodePool | default "" | toString | trim }}
{{- if $pool }}
nodePool: {{ $pool | quote }}
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
{{- if $v.hostnames }}
hostnames:
{{ toYaml $v.hostnames | indent 2 }}
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
{{- if $v.rollout }}
rollout:
{{ toYaml $v.rollout | indent 2 }}
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
{{- $knownKeys := list "name" "owner" "image" "repo" "tag" "imageTag" "dockerfile" "port" "containerPort" "replicas" "hpa" "resources" "pool" "nodePool" "hostname" "hostnames" "expose" "metrics" "traffic" "readinessProbe" "livenessProbe" "singleton" "maxDown" "shutdown" "canary" "rollout" "injectPipeline" "route" "routes" "environment" -}}
{{- $state := dict "hasInstance" false -}}
{{- range $k, $val := . -}}
  {{- /* `_`-prefixed keys are YAML anchor bases (e.g. _common: &common), not instances. */ -}}
  {{- if and (not (has $k $knownKeys)) (not (hasPrefix "_" $k)) (kindIs "map" $val) -}}
    {{- $_ := set $state "hasInstance" true -}}
  {{- end -}}
{{- end -}}
{{- if $state.hasInstance -}}true{{- else -}}false{{- end -}}
{{- end -}}

{{/*
Resolve a tenant's route name(s) into the CR route object
{group, primary, weighted, backends, entries}.
Args (dict): rnames (list of route names), instance (this instance's key),
primaryOf (route name -> primary instance), catalog (merged route policies),
release (Helm release name), membersOf (pool -> instance names),
weightOf (instance -> weight), weightedPools (set of pools that declared weights).
The primary route is the first name; group is <release>-<primaryRoute>. Only the
primary emits entries (the HTTPRoutes) and backends (the weighted split).

Backends name the BirService of each member (<release>-<instance>); the operator derives
that member's Service from the name. Members iterate in the order they were collected from
the sorted values map, so the list is stable across renders (an unstable order would show
up as a permanent ArgoCD diff).
*/}}
{{- define "birservice.resolveRoute" -}}
{{- $rnames := .rnames -}}
{{- /* Route names become part of Service/HTTPRoute object names, which must be DNS
       labels — no dots, underscores, or uppercase. Fail early with a clear message. */ -}}
{{- range $rn := $rnames -}}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]*[a-z0-9])?$" ($rn | toString)) -}}
{{- fail (printf "route name %q must be a DNS label (lowercase letters, digits, hyphens; no dots/underscores)" $rn) -}}
{{- end -}}
{{- end -}}
{{- $primaryRoute := index $rnames 0 | toString -}}
{{- $isPrimary := eq (get .primaryOf $primaryRoute) .instance -}}
{{- $isWeighted := hasKey (.weightedPools | default dict) $primaryRoute -}}
group: {{ printf "%s-%s" .release $primaryRoute }}
{{- /* Emit `primary`/`weighted` only when true. Both CR fields are `bool omitempty`, so
       a stored `false` serializes back as absent — emitting `false` here would make
       ArgoCD diff desired(false) vs live(absent) forever (perpetual OutOfSync). The
       operator reads absent as false, so omitting is equivalent and clean. */ -}}
{{- if $isWeighted }}
weighted: true
{{- end }}
{{- if $isPrimary }}
primary: true
{{- if $isWeighted }}
backends:
{{- range $m := index $.membersOf $primaryRoute }}
  - name: {{ printf "%s-%s" $.release $m }}
    weight: {{ index $.weightOf $m }}
{{- end }}
{{- end }}
entries:
{{- range $rn := $rnames }}
{{- $rn = $rn | toString }}
{{- $def := get $.catalog $rn | default dict }}
  - name: {{ $rn }}
{{- if $def.hostname }}
    hostname: {{ $def.hostname }}
{{- end }}
{{- if $def.timeout }}
    timeout: {{ $def.timeout | toString | quote }}
{{- end }}
{{- if hasKey $def "retries" }}
    retries: {{ $def.retries }}
{{- end }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Common labels for the BirService CR. Owner is propagated as both label (sanitized) and annotation (raw).
*/}}
{{- define "birservice.labels" -}}
app.kubernetes.io/managed-by: "argocd"
deploy.easydeploy.io/tenant: {{ .Release.Namespace | quote }}
{{- end -}}
