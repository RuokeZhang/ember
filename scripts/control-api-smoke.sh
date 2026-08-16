#!/usr/bin/env bash
set -euo pipefail

KUBECTL=${KUBECTL:-kubectl}
PORT=${PORT:-18082}
SYSTEM_NAMESPACE=ember-system
forward_pid=
endpoint_id=
cookie_jar=$(mktemp)
forward_log=$(mktemp)
response_body=$(mktemp)
idempotency_key="control-smoke-$(date +%s)-${RANDOM}"

start_forward() {
  "${KUBECTL}" -n "${SYSTEM_NAMESPACE}" port-forward service/ember-control-api "${PORT}:8080" >"${forward_log}" 2>&1 &
  forward_pid=$!
  for _ in $(seq 1 30); do
    curl -fsS "http://127.0.0.1:${PORT}/readyz" >/dev/null 2>&1 && return
    sleep 1
  done
  cat "${forward_log}" >&2
  echo "control API port-forward did not become ready" >&2
  exit 1
}

stop_forward() {
  if [[ -n "${forward_pid}" ]]; then
    kill "${forward_pid}" >/dev/null 2>&1 || true
    wait "${forward_pid}" 2>/dev/null || true
    forward_pid=
  fi
}

cleanup() {
  stop_forward
  rm -f "${cookie_jar}" "${forward_log}" "${response_body}"
  if [[ -n "${endpoint_id}" ]]; then
    "${KUBECTL}" -n "${SYSTEM_NAMESPACE}" delete inferenceendpoint "${endpoint_id}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" rollout status statefulset/ember-postgres --timeout=180s
"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" rollout status deployment/ember-control-api --timeout=180s
start_forward

web_root=$(curl -fsS "http://127.0.0.1:${PORT}/")
if [[ "${web_root}" != *"GPU inference control plane"* ]]; then
  echo "control API did not serve the Ember product UI" >&2
  exit 1
fi
asset_path=$(grep -o 'src="/assets/[^"]*"' <<<"${web_root}" | head -n 1 | cut -d'"' -f2)
if [[ -z "${asset_path}" ]]; then
  echo "product UI did not reference a built JavaScript asset" >&2
  exit 1
fi
curl -fsS "http://127.0.0.1:${PORT}${asset_path}" >/dev/null

session=$(curl -fsS -c "${cookie_jar}" -X POST "http://127.0.0.1:${PORT}/api/v1/session")
owner_id=$(jq -r '.ownerID' <<<"${session}")
if [[ -z "${owner_id}" || "${owner_id}" == "null" ]]; then
  echo "control API did not issue a demo owner session: ${session}" >&2
  exit 1
fi

catalog=$(curl -fsS -b "${cookie_jar}" "http://127.0.0.1:${PORT}/api/v1/catalog/models")
if [[ "$(jq -r '.models[0].id' <<<"${catalog}")" != "qwen2.5-7b-instruct-awq" ]]; then
  echo "control API catalog did not expose the reviewed model" >&2
  exit 1
fi

create_payload='{
  "displayName":"Control API smoke",
  "modelID":"qwen2.5-7b-instruct-awq",
  "profile":"standard",
  "maxReplicas":3,
  "idleTimeoutSeconds":300,
  "cachePreference":"Preferred"
}'
create_code=$(curl -sS -b "${cookie_jar}" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: ${idempotency_key}" \
  -o "${response_body}" -w '%{http_code}' \
  -d "${create_payload}" \
  "http://127.0.0.1:${PORT}/api/v1/endpoints")
if [[ "${create_code}" != "201" ]]; then
  echo "control API endpoint create failed (${create_code}): $(cat "${response_body}")" >&2
  exit 1
fi
endpoint_id=$(jq -r '.id' "${response_body}")
if [[ -z "${endpoint_id}" || "${endpoint_id}" == "null" ]]; then
  echo "control API did not return an endpoint ID" >&2
  exit 1
fi

replay_code=$(curl -sS -b "${cookie_jar}" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: ${idempotency_key}" \
  -o "${response_body}" -w '%{http_code}' \
  -d "${create_payload}" \
  "http://127.0.0.1:${PORT}/api/v1/endpoints")
if [[ "${replay_code}" != "200" || "$(jq -r '.id' "${response_body}")" != "${endpoint_id}" ]]; then
  echo "idempotent create replay did not return the original endpoint" >&2
  exit 1
fi

conflict_payload=${create_payload/Control API smoke/Conflicting request}
conflict_code=$(curl -sS -b "${cookie_jar}" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: ${idempotency_key}" \
  -o "${response_body}" -w '%{http_code}' \
  -d "${conflict_payload}" \
  "http://127.0.0.1:${PORT}/api/v1/endpoints")
if [[ "${conflict_code}" != "409" ]]; then
  echo "idempotency conflict was not rejected: ${conflict_code} $(cat "${response_body}")" >&2
  exit 1
fi

"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" wait --for=condition=Ready "inferenceendpoint/${endpoint_id}" --timeout=180s
cr_owner=$("${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get inferenceendpoint "${endpoint_id}" -o jsonpath='{.spec.ownerID}')
if [[ "${cr_owner}" != "${owner_id}" ]]; then
  echo "gateway did not bind the CR to the session owner" >&2
  exit 1
fi

for _ in $(seq 1 60); do
  endpoint=$(curl -fsS -b "${cookie_jar}" "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}")
  [[ "$(jq -r '.runtime.phase' <<<"${endpoint}")" == "Ready" ]] && break
  sleep 1
done
if [[ "$(jq -r '.runtime.phase' <<<"${endpoint}")" != "Ready" ]]; then
  echo "control API did not project the authoritative Ready status: ${endpoint}" >&2
  exit 1
fi

inspection=$(curl -fsS -b "${cookie_jar}" "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}/inspect")
if ! jq -e '
  (.namespace | startswith("ember-ep-")) and
  (.resources | any(.kind == "Deployment")) and
  (.resources | any(.kind == "ScaledObject")) and
  (.resources | any(.kind == "ResourceQuota")) and
  (.pods | any(.requestedGPUs == 1)) and
  (.securityControls | length >= 4)
' >/dev/null <<<"${inspection}"; then
  echo "owner-scoped Kubernetes inspection is incomplete: ${inspection}" >&2
  exit 1
fi
workload_namespace=$(jq -r '.namespace' <<<"${inspection}")

metrics=$(curl -fsS -b "${cookie_jar}" "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}/metrics?window=300&step=5")
if ! jq -e '
  .windowSeconds == 300 and
  .stepSeconds == 5 and
  (.current.replicas | type == "number") and
  (.series | type == "array")
' >/dev/null <<<"${metrics}"; then
  echo "Prometheus endpoint metrics are incomplete: ${metrics}" >&2
  exit 1
fi

completion=$(curl -fsS -b "${cookie_jar}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-7b-instruct-awq","messages":[{"role":"user","content":"Verify the Control API path."}]}' \
  "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}/v1/chat/completions")
if [[ "${completion}" != *"Ember reconciled this GPU inference endpoint successfully."* ]]; then
  echo "unexpected inference response through Control API: ${completion}" >&2
  exit 1
fi

stream_completion=$(curl -fsS -N -b "${cookie_jar}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-7b-instruct-awq","stream":true,"messages":[{"role":"user","content":"Verify the streaming Control API path."}]}' \
  "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}/v1/chat/completions")
if [[ "${stream_completion}" != *"data:"* || "${stream_completion}" != *"[DONE]"* ]]; then
  echo "unexpected streaming inference response: ${stream_completion}" >&2
  exit 1
fi

load_pids=()
for _ in $(seq 1 8); do
  curl -fsS -b "${cookie_jar}" \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen2.5-7b-instruct-awq","messages":[{"role":"user","content":"Drive the browser Load Lab."}]}' \
    "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}/v1/chat/completions" \
    >/dev/null &
  load_pids+=("$!")
done
scaled_up=false
for _ in $(seq 1 90); do
  desired_replicas=$("${KUBECTL}" -n "${workload_namespace}" get deployment engine -o jsonpath='{.spec.replicas}')
  ready_replicas=$("${KUBECTL}" -n "${workload_namespace}" get deployment engine -o jsonpath='{.status.readyReplicas}')
  if [[ "${desired_replicas:-0}" -ge 2 && "${ready_replicas:-0}" -ge 2 ]]; then
    scaled_up=true
    break
  fi
  sleep 1
done
for load_pid in "${load_pids[@]}"; do
  wait "${load_pid}"
done
if [[ "${scaled_up}" != "true" ]]; then
  echo "browser-facing load path did not trigger KEDA scale-up" >&2
  exit 1
fi

logs=$(curl -fsS -b "${cookie_jar}" "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}/logs?tail=50")
if [[ -z "${logs}" ]]; then
  echo "bounded engine logs were empty" >&2
  exit 1
fi

events=$(curl -fsS -b "${cookie_jar}" "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}/events")
if ! jq -e --arg owner "${owner_id}" '.events[] | select(
  .actor == $owner and
  .action == "endpoint.create" and
  .endpointUID != "" and
  .requestID != "" and
  .result == "succeeded" and
  .createdAt != null
)' >/dev/null <<<"${events}"; then
  echo "successful create audit event is missing required fields" >&2
  exit 1
fi
if ! jq -e '.events[] | select(.action == "endpoint.inference" and .result == "http_200")' >/dev/null <<<"${events}"; then
  echo "inference audit event was not recorded" >&2
  exit 1
fi
if ! jq -e '[.events[] | select(.action == "endpoint.metrics" or .action == "endpoint.inspect")] | length == 0' >/dev/null <<<"${events}"; then
  echo "telemetry polling polluted the append-only audit history" >&2
  exit 1
fi
if "${KUBECTL}" -n "${SYSTEM_NAMESPACE}" exec statefulset/ember-postgres -- \
  sh -ec 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "UPDATE audit_events SET result = '\''tampered'\'' WHERE id = (SELECT max(id) FROM audit_events)"' \
  >/dev/null 2>&1; then
  echo "append-only audit history accepted an UPDATE" >&2
  exit 1
fi

stop_forward
"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" delete pod ember-postgres-0 --wait=true
"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" rollout status statefulset/ember-postgres --timeout=180s
"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" rollout restart deployment/ember-control-api
"${KUBECTL}" -n "${SYSTEM_NAMESPACE}" rollout status deployment/ember-control-api --timeout=180s
start_forward

restored_session=$(curl -fsS -b "${cookie_jar}" "http://127.0.0.1:${PORT}/api/v1/session")
restored_endpoint=$(curl -fsS -b "${cookie_jar}" "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}")
if [[ "$(jq -r '.ownerID' <<<"${restored_session}")" != "${owner_id}" || "$(jq -r '.id' <<<"${restored_endpoint}")" != "${endpoint_id}" ]]; then
  echo "session or endpoint metadata did not survive Postgres and API restarts" >&2
  exit 1
fi

delete_code=$(curl -sS -b "${cookie_jar}" -X DELETE \
  -o "${response_body}" -w '%{http_code}' \
  "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}")
if [[ "${delete_code}" != "202" && "${delete_code}" != "200" ]]; then
  echo "control API delete failed (${delete_code}): $(cat "${response_body}")" >&2
  exit 1
fi

for _ in $(seq 1 120); do
  deleted=$(curl -fsS -b "${cookie_jar}" "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}")
  [[ "$(jq -r '.runtime.phase' <<<"${deleted}")" == "Deleted" ]] && break
  sleep 1
done
if [[ "$(jq -r '.runtime.phase' <<<"${deleted}")" != "Deleted" || "$(jq -r '.modelID' <<<"${deleted}")" != "qwen2.5-7b-instruct-awq" ]]; then
  echo "deleted CR metadata was not retained in Postgres: ${deleted}" >&2
  exit 1
fi
deleted_events=$(curl -fsS -b "${cookie_jar}" "http://127.0.0.1:${PORT}/api/v1/endpoints/${endpoint_id}/events")
if ! jq -e '.events[] | select(.action == "endpoint.delete" and (.result == "accepted" or .result == "succeeded"))' >/dev/null <<<"${deleted_events}"; then
  echo "delete audit history did not survive CR reclamation" >&2
  exit 1
fi

post_delete_replay_code=$(curl -sS -b "${cookie_jar}" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: ${idempotency_key}" \
  -o "${response_body}" -w '%{http_code}' \
  -d "${create_payload}" \
  "http://127.0.0.1:${PORT}/api/v1/endpoints")
if [[ "${post_delete_replay_code}" != "200" || "$(jq -r '.runtime.phase' "${response_body}")" != "Deleted" ]]; then
  echo "deleted endpoint idempotency record was not retained" >&2
  exit 1
fi
if "${KUBECTL}" -n "${SYSTEM_NAMESPACE}" get inferenceendpoint "${endpoint_id}" >/dev/null 2>&1; then
  echo "idempotency replay unexpectedly recreated a deleted CR" >&2
  exit 1
fi

endpoint_id=
echo "Ember Control API smoke test passed."
