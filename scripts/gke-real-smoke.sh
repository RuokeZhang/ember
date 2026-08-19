#!/usr/bin/env bash
set -euo pipefail

GCLOUD=${GCLOUD:-gcloud}
KUBECTL=${KUBECTL:-kubectl}
CURL=${CURL:-curl}
PROJECT_ID=${PROJECT_ID:-}
CLUSTER_NAME=${CLUSTER_NAME:-ember-gpu}
CLUSTER_LOCATION=${CLUSTER_LOCATION:-us-central1-a}
GPU_NODE_POOL=${GPU_NODE_POOL:-l4-spot}
ENDPOINT_NAME=ep-7f92c8
LOCAL_PORT=${LOCAL_PORT:-18000}
KEEP_RESOURCES=${KEEP_RESOURCES:-false}

usage() {
  cat <<'EOF'
Usage: scripts/gke-real-smoke.sh

Start one L4 node, verify a real vLLM chat completion, then remove the endpoint
and resize the GPU pool to zero unless KEEP_RESOURCES=true.
EOF
}

die() {
  echo "gke real smoke: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

case "${1:-}" in
-h | --help | help)
  usage
  exit 0
  ;;
"")
  ;;
*)
  usage >&2
  exit 1
  ;;
esac

for command in "${GCLOUD}" "${KUBECTL}" gke-gcloud-auth-plugin "${CURL}" jq; do
  require_command "${command}"
done

if [[ -z "${PROJECT_ID}" ]]; then
  PROJECT_ID=$("${GCLOUD}" config get-value project 2>/dev/null || true)
fi
[[ "${PROJECT_ID}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "invalid PROJECT_ID"
[[ "${LOCAL_PORT}" =~ ^[1-9][0-9]{0,4}$ ]] || die "invalid LOCAL_PORT"
if ((LOCAL_PORT > 65535)); then
  die "LOCAL_PORT must not exceed 65535"
fi
if [[ "${KEEP_RESOURCES}" != "true" && "${KEEP_RESOURCES}" != "false" && "${KEEP_RESOURCES}" != "1" && "${KEEP_RESOURCES}" != "0" ]]; then
  die "KEEP_RESOURCES must be true or false"
fi

"${GCLOUD}" container clusters get-credentials "${CLUSTER_NAME}" \
  --project="${PROJECT_ID}" \
  --location="${CLUSTER_LOCATION}" >/dev/null

if "${KUBECTL}" -n ember-system get inferenceendpoint "${ENDPOINT_NAME}" >/dev/null 2>&1; then
  die "endpoint ${ENDPOINT_NAME} already exists; refusing to overwrite it"
fi

created_endpoint=false
gpu_started=false
port_forward_pid=
port_forward_log=$(mktemp)
response_file=$(mktemp)

cleanup() {
  local exit_code=$?
  local cleanup_failed=false
  trap - EXIT
  if [[ -n "${port_forward_pid}" ]]; then
    kill "${port_forward_pid}" >/dev/null 2>&1 || true
    wait "${port_forward_pid}" >/dev/null 2>&1 || true
  fi
  rm -f "${port_forward_log}" "${response_file}"
  if [[ "${KEEP_RESOURCES}" != "true" && "${KEEP_RESOURCES}" != "1" ]]; then
    if [[ "${created_endpoint}" == "true" ]]; then
      if ! "${KUBECTL}" -n ember-system delete inferenceendpoint "${ENDPOINT_NAME}" --ignore-not-found --wait --timeout=5m; then
        cleanup_failed=true
      fi
    fi
    if [[ "${gpu_started}" == "true" ]]; then
      if ! "${GCLOUD}" container clusters resize "${CLUSTER_NAME}" \
        --node-pool="${GPU_NODE_POOL}" \
        --num-nodes=0 \
        --project="${PROJECT_ID}" \
        --location="${CLUSTER_LOCATION}" \
        --quiet; then
        cleanup_failed=true
      fi
    fi
  fi
  if [[ "${cleanup_failed}" == "true" ]]; then
    echo "Automatic cleanup was incomplete; the armed Cloud Tasks TTL remains active." >&2
    if ((exit_code == 0)); then
      exit_code=1
    fi
  fi
  exit "${exit_code}"
}
trap cleanup EXIT

PROJECT_ID="${PROJECT_ID}" \
  CLUSTER_NAME="${CLUSTER_NAME}" \
  CLUSTER_LOCATION="${CLUSTER_LOCATION}" \
  GPU_NODE_POOL="${GPU_NODE_POOL}" \
  ./scripts/gcp-cost-guard.sh arm
if gateways=$("${KUBECTL}" get gateways.gateway.networking.k8s.io -A -o json 2>/dev/null) &&
  jq -e 'any(.items[]?; .spec.gatewayClassName == "gke-l7-global-external-managed")' \
    <<<"${gateways}" >/dev/null; then
  PROJECT_ID="${PROJECT_ID}" \
    CLUSTER_NAME="${CLUSTER_NAME}" \
    CLUSTER_LOCATION="${CLUSTER_LOCATION}" \
    GPU_NODE_POOL="${GPU_NODE_POOL}" \
    ./scripts/gcp-cost-guard.sh keep-cluster
  echo "Public Gateway detected; retained the GPU-pool timer but removed the cluster deletion timer."
fi
"${GCLOUD}" container clusters resize "${CLUSTER_NAME}" \
  --node-pool="${GPU_NODE_POOL}" \
  --num-nodes=1 \
  --project="${PROJECT_ID}" \
  --location="${CLUSTER_LOCATION}" \
  --quiet
gpu_started=true
gpu_node=
for _ in $(seq 1 120); do
  gpu_node=$("${KUBECTL}" get nodes -l ember.dev/gpu=l4 -o name | sed -n '1p')
  if [[ -n "${gpu_node}" ]]; then
    break
  fi
  sleep 5
done
[[ -n "${gpu_node}" ]] || die "the L4 node did not register within 10 minutes"
"${KUBECTL}" wait --for=condition=Ready "${gpu_node}" --timeout=10m

"${KUBECTL}" apply -f operator/config/samples/serving_v1alpha1_inferenceendpoint.yaml
created_endpoint=true

echo "Waiting for immutable model download, cache verification, and vLLM startup."
"${KUBECTL}" -n ember-system wait \
  --for=condition=Ready \
  "inferenceendpoint/${ENDPOINT_NAME}" \
  --timeout=30m

workload_namespace=$("${KUBECTL}" -n ember-system get inferenceendpoint "${ENDPOINT_NAME}" -o jsonpath='{.status.workloadNamespace}')
[[ "${workload_namespace}" =~ ^ember-ep-[a-f0-9]{8}$ ]] || die "endpoint reported invalid workload namespace ${workload_namespace}"
"${KUBECTL}" -n "${workload_namespace}" rollout status deployment/engine --timeout=10m
"${KUBECTL}" -n "${workload_namespace}" get pods -l component=engine -o wide

"${KUBECTL}" -n "${workload_namespace}" port-forward service/engine "${LOCAL_PORT}:8000" >"${port_forward_log}" 2>&1 &
port_forward_pid=$!
for _ in $(seq 1 30); do
  if "${CURL}" -fsS "http://127.0.0.1:${LOCAL_PORT}/health" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "${port_forward_pid}" 2>/dev/null; then
    cat "${port_forward_log}" >&2
    die "kubectl port-forward exited"
  fi
  sleep 1
done
"${CURL}" -fsS "http://127.0.0.1:${LOCAL_PORT}/health" >/dev/null || die "vLLM health endpoint did not become reachable"

latency=$("${CURL}" -fsS \
  -o "${response_file}" \
  -w '%{time_total}' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-7b-instruct-awq","messages":[{"role":"user","content":"Reply with exactly: Ember L4 validation succeeded."}],"temperature":0,"max_tokens":32}' \
  "http://127.0.0.1:${LOCAL_PORT}/v1/chat/completions")

jq -e '.choices[0].message.content | type == "string" and length > 0' "${response_file}" >/dev/null
jq '{id,model,content:.choices[0].message.content,usage}' "${response_file}"
echo "Single smoke-request wall time: ${latency}s (not a benchmark)."

if [[ "${KEEP_RESOURCES}" == "true" || "${KEEP_RESOURCES}" == "1" ]]; then
  echo "KEEP_RESOURCES is enabled; the endpoint and GPU pool remain protected by the existing TTL tasks."
else
  echo "Validation succeeded; cleanup will delete the endpoint and resize the GPU pool to zero."
fi
