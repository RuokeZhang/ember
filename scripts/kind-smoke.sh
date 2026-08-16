#!/usr/bin/env bash
set -euo pipefail

KUBECTL=${KUBECTL:-kubectl}
PORT=${PORT:-18080}
GO_IMAGE=${GO_IMAGE:-golang:1.24}
NAME=ep-smoke
WARM_NAME=ep-smoke-warm
SYSTEM_NAMESPACE=ember-system
ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
forward_pid=
forward_log=$(mktemp)

issue_token() {
  local subject=$1
  "${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get secret ember-jwt-keys -o jsonpath='{.data.private\.key}' |
    docker run --rm -i --user "$(id -u):$(id -g)" \
      -e GOMODCACHE=/workspace/.cache/gomod \
      -e GOCACHE=/workspace/.cache/gocache \
      -v "${ROOT_DIR}:/workspace" \
      -w /workspace \
      "${GO_IMAGE}" \
      go run ./cmd/auth-tool token \
      --private-key-base64-stdin \
      --subject "${subject}" \
      --ttl 120s
}

cleanup() {
  if [[ -n "${forward_pid}" ]]; then
    kill "${forward_pid}" >/dev/null 2>&1 || true
  fi
  rm -f "${forward_log}"
  "${KUBECTL}" -n "${SYSTEM_NAMESPACE}" delete inferenceendpoint "${NAME}" "${WARM_NAME}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

"${KUBECTL}" apply -f - <<EOF
apiVersion: serving.ember.dev/v1alpha1
kind: InferenceEndpoint
metadata:
  name: ${NAME}
  namespace: ${SYSTEM_NAMESPACE}
spec:
  ownerID: smoke_runner
  model:
    id: qwen2.5-7b-instruct-awq
    revision: 9c1f4ae
  profile: standard
  scaling:
    minReplicas: 0
    maxReplicas: 3
    targetQueueDepth: 4
    idleTimeoutSeconds: 300
  placement:
    cachePreference: Preferred
    maxColdStartFallbackSeconds: 30
EOF

"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" wait --for=condition=Ready "inferenceendpoint/${NAME}" --timeout=180s
workload_namespace=$("${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get inferenceendpoint "${NAME}" -o jsonpath='{.status.workloadNamespace}')
if [[ -z "${workload_namespace}" ]]; then
  echo "endpoint did not report a workload namespace" >&2
  exit 1
fi

gpu_request=$("${KUBECTL}" -n "${workload_namespace}" get pod -l component=engine -o jsonpath='{.items[0].spec.containers[0].resources.requests.nvidia\.com/gpu}')
if [[ "${gpu_request}" != "1" ]]; then
  echo "expected one simulated GPU, got ${gpu_request}" >&2
  exit 1
fi
cache_hash=$("${KUBECTL}" -n "${workload_namespace}" get pod -l component=engine -o jsonpath='{.items[0].metadata.labels.ember\.dev/cache-hash}')
node_name=$("${KUBECTL}" -n "${workload_namespace}" get pod -l component=engine -o jsonpath='{.items[0].spec.nodeName}')
affinity_node=$("${KUBECTL}" -n "${workload_namespace}" get pod -l component=engine -o jsonpath='{.items[0].spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]}')
cache_path=$("${KUBECTL}" -n "${workload_namespace}" get pod -l component=engine -o jsonpath='{.items[0].spec.volumes[?(@.name=="model-cache")].hostPath.path}')
cache_read_only=$("${KUBECTL}" -n "${workload_namespace}" get pod -l component=engine -o jsonpath='{.items[0].spec.containers[0].volumeMounts[?(@.name=="model-cache")].readOnly}')
verify_exit=$("${KUBECTL}" -n "${workload_namespace}" get pod -l component=engine -o jsonpath='{.items[0].status.initContainerStatuses[?(@.name=="verify-cache")].state.terminated.exitCode}')
if [[ -z "${cache_hash}" || "${affinity_node}" != "${node_name}" || "${cache_path}" != "/var/lib/ember/models/${cache_hash}" || "${cache_read_only}" != "true" || "${verify_exit}" != "0" ]]; then
  echo "cache-aware placement validation failed for ${workload_namespace}" >&2
  exit 1
fi
"${KUBECTL}" wait --for=condition=Ready "modelcache/mc-${cache_hash}" --timeout=60s
prefetch_uid_before=$("${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get jobs -l "ember.dev/cache-hash=${cache_hash}" -o jsonpath='{range .items[*]}{.metadata.uid}{end}')

"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" wait --for=condition=Available deployment/ember-gateway --timeout=60s
"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" port-forward "service/ember-gateway" "${PORT}:8080" >"${forward_log}" 2>&1 &
forward_pid=$!
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1 && break
  sleep 1
done

token=$(issue_token smoke_runner)
completion=$(curl -fsS "http://127.0.0.1:${PORT}/v1/endpoints/${NAME}/v1/chat/completions" \
  -H "Authorization: Bearer ${token}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-7b-instruct-awq","messages":[{"role":"user","content":"Smoke test Ember."}]}')
if [[ "${completion}" != *"Ember reconciled this GPU inference endpoint successfully."* ]]; then
  echo "unexpected completion response: ${completion}" >&2
  exit 1
fi

"${KUBECTL}" -n "${workload_namespace}" delete service engine
for _ in $(seq 1 60); do
  "${KUBECTL}" -n "${workload_namespace}" get service engine >/dev/null 2>&1 && break
  sleep 1
done
"${KUBECTL}" -n "${workload_namespace}" get service engine >/dev/null
old_pod=$("${KUBECTL}" -n "${workload_namespace}" get pod -l component=engine -o jsonpath='{.items[0].metadata.name}')
"${KUBECTL}" -n "${workload_namespace}" delete pod "${old_pod}"
for _ in $(seq 1 60); do
  new_pod=$("${KUBECTL}" -n "${workload_namespace}" get pod -l component=engine -o jsonpath='{.items[0].metadata.name}')
  [[ -n "${new_pod}" && "${new_pod}" != "${old_pod}" ]] && break
  sleep 1
done
"${KUBECTL}" -n "${workload_namespace}" rollout status deployment/engine --timeout=120s
for _ in $(seq 1 60); do
  ready_reason=$("${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get inferenceendpoint "${NAME}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')
  ready_replicas=$("${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get inferenceendpoint "${NAME}" -o jsonpath='{.status.replicas.ready}')
  [[ "${ready_reason}" == "EngineServing" && "${ready_replicas:-0}" -ge 1 ]] && break
  sleep 1
done
if [[ "${ready_reason}" != "EngineServing" || "${ready_replicas:-0}" -lt 1 ]]; then
  echo "endpoint status did not recover after deployment recreation" >&2
  exit 1
fi

"${KUBECTL}" -n "${workload_namespace}" wait --for=condition=Ready scaledobject/engine-autoscaler --timeout=60s
"${KUBECTL}" -n "${workload_namespace}" get hpa keda-hpa-engine-autoscaler >/dev/null
load_token=$(issue_token smoke_runner)
load_pids=()
for _ in $(seq 1 8); do
  curl -fsS "http://127.0.0.1:${PORT}/v1/endpoints/${NAME}/v1/chat/completions" \
    -H "Authorization: Bearer ${load_token}" \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen2.5-7b-instruct-awq","messages":[{"role":"user","content":"Drive the KEDA queue."}]}' \
    >/dev/null &
  load_pids+=("$!")
done
scaled_up=false
for _ in $(seq 1 90); do
  desired_replicas=$("${KUBECTL}" -n "${workload_namespace}" get deployment engine -o jsonpath='{.spec.replicas}')
  ready_replicas=$("${KUBECTL}" -n "${workload_namespace}" get deployment engine -o jsonpath='{.status.readyReplicas}')
  if [[ "${desired_replicas}" -ge 2 && "${ready_replicas:-0}" -ge 2 ]]; then
    scaled_up=true
    break
  fi
  sleep 1
done
for load_pid in "${load_pids[@]}"; do
  wait "${load_pid}"
done
if [[ "${scaled_up}" != "true" ]]; then
  echo "KEDA did not scale the engine from one to two replicas" >&2
  exit 1
fi

"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" patch inferenceendpoint "${NAME}" --subresource=status --type=merge \
  -p '{"status":{"lastActivityTime":"2020-01-01T00:00:00Z"}}'
ready_reason=
for _ in $(seq 1 60); do
  ready_reason=$("${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get inferenceendpoint "${NAME}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')
  [[ "${ready_reason}" == "ScaledToZero" ]] && break
  sleep 1
done
if [[ "${ready_reason}" != "ScaledToZero" ]]; then
  echo "endpoint did not scale to zero" >&2
  exit 1
fi
paused=$("${KUBECTL}" -n "${workload_namespace}" get scaledobject engine-autoscaler -o jsonpath='{.metadata.annotations.autoscaling\.keda\.sh/paused}')
desired_replicas=$("${KUBECTL}" -n "${workload_namespace}" get deployment engine -o jsonpath='{.spec.replicas}')
if [[ "${paused}" != "true" || "${desired_replicas}" != "0" ]]; then
  echo "idle endpoint did not pause KEDA at zero" >&2
  exit 1
fi

activation_token=$(issue_token smoke_runner)
activation_headers=$(mktemp)
activation_body=$(mktemp)
activation_code=$(curl -sS -D "${activation_headers}" -o "${activation_body}" -w '%{http_code}' \
  "http://127.0.0.1:${PORT}/v1/endpoints/${NAME}/v1/chat/completions" \
  -H "Authorization: Bearer ${activation_token}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-7b-instruct-awq","messages":[{"role":"user","content":"Wake Ember."}]}')
retry_after=$(awk 'tolower($1)=="retry-after:" {gsub("\r", "", $2); print $2}' "${activation_headers}")
rm -f "${activation_headers}" "${activation_body}"
if [[ "${activation_code}" != "503" || "${retry_after}" != "5" ]]; then
  echo "gateway did not return the scale-from-zero retry contract" >&2
  exit 1
fi
"${KUBECTL}" -n "${workload_namespace}" rollout status deployment/engine --timeout=120s
for _ in $(seq 1 60); do
  ready_reason=$("${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get inferenceendpoint "${NAME}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')
  activation=$("${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get inferenceendpoint "${NAME}" -o jsonpath='{.metadata.annotations.serving\.ember\.dev/activate-at}')
  paused=$("${KUBECTL}" -n "${workload_namespace}" get scaledobject engine-autoscaler -o jsonpath='{.metadata.annotations.autoscaling\.keda\.sh/paused}')
  [[ "${ready_reason}" == "EngineServing" && -z "${activation}" && -z "${paused}" ]] && break
  sleep 1
done
if [[ "${ready_reason}" != "EngineServing" ]]; then
  echo "endpoint did not reactivate" >&2
  exit 1
fi
completion=$(curl -fsS "http://127.0.0.1:${PORT}/v1/endpoints/${NAME}/v1/chat/completions" \
  -H "Authorization: Bearer ${activation_token}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-7b-instruct-awq","messages":[{"role":"user","content":"Confirm Ember is awake."}]}')
if [[ "${completion}" != *"Ember reconciled this GPU inference endpoint successfully."* ]]; then
  echo "unexpected post-activation completion response: ${completion}" >&2
  exit 1
fi

"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" delete inferenceendpoint "${NAME}" --wait=false
"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" wait --for=delete "inferenceendpoint/${NAME}" --timeout=120s
"${KUBECTL}" wait --for=delete "namespace/${workload_namespace}" --timeout=120s

"${KUBECTL}" apply -f - <<EOF
apiVersion: serving.ember.dev/v1alpha1
kind: InferenceEndpoint
metadata:
  name: ${WARM_NAME}
  namespace: ${SYSTEM_NAMESPACE}
spec:
  ownerID: smoke_warm_runner
  model:
    id: qwen2.5-7b-instruct-awq
    revision: 9c1f4ae
  profile: standard
  scaling:
    minReplicas: 0
    maxReplicas: 3
    targetQueueDepth: 4
    idleTimeoutSeconds: 300
  placement:
    cachePreference: Required
    maxColdStartFallbackSeconds: 30
EOF

"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" wait --for=condition=Ready "inferenceendpoint/${WARM_NAME}" --timeout=120s
warm_namespace=$("${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get inferenceendpoint "${WARM_NAME}" -o jsonpath='{.status.workloadNamespace}')
warm_node=$("${KUBECTL}" -n "${warm_namespace}" get pod -l component=engine -o jsonpath='{.items[0].spec.nodeName}')
warm_cache_path=$("${KUBECTL}" -n "${warm_namespace}" get pod -l component=engine -o jsonpath='{.items[0].spec.volumes[?(@.name=="model-cache")].hostPath.path}')
warm_cache_read_only=$("${KUBECTL}" -n "${warm_namespace}" get pod -l component=engine -o jsonpath='{.items[0].spec.containers[0].volumeMounts[?(@.name=="model-cache")].readOnly}')
if [[ "${warm_node}" != "${node_name}" || "${warm_cache_path}" != "${cache_path}" || "${warm_cache_read_only}" != "true" ]]; then
  echo "warm endpoint did not reuse the verified cache on ${node_name}" >&2
  exit 1
fi
prefetch_uid_after=$("${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get jobs -l "ember.dev/cache-hash=${cache_hash}" -o jsonpath='{range .items[*]}{.metadata.uid}{end}')
if [[ -z "${prefetch_uid_before}" && -n "${prefetch_uid_after}" ]]; then
  echo "warm endpoint unexpectedly created a prefetch job" >&2
  exit 1
fi
if [[ -n "${prefetch_uid_before}" && -n "${prefetch_uid_after}" && "${prefetch_uid_before}" != "${prefetch_uid_after}" ]]; then
  echo "warm endpoint replaced the existing prefetch job" >&2
  exit 1
fi

"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" delete inferenceendpoint "${WARM_NAME}" --wait=false
"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" wait --for=delete "inferenceendpoint/${WARM_NAME}" --timeout=120s
"${KUBECTL}" wait --for=delete "namespace/${warm_namespace}" --timeout=120s

kill "${forward_pid}" >/dev/null 2>&1 || true
wait "${forward_pid}" 2>/dev/null || true
forward_pid=
trap - EXIT
rm -f "${forward_log}"
echo "Ember Kind smoke test passed."
