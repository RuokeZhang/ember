#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "${REPO_ROOT}"

GCLOUD=${GCLOUD:-gcloud}
KUBECTL=${KUBECTL:-kubectl}
CURL=${CURL:-curl}
DIG=${DIG:-dig}
DOCKER=${DOCKER:-docker}

PROJECT_ID=${PROJECT_ID:-}
CLUSTER_NAME=${CLUSTER_NAME:-ember-gpu}
CLUSTER_LOCATION=${CLUSTER_LOCATION:-us-central1-a}
GATEWAY_HOST=${GATEWAY_HOST:-api.ember.ruokezhang.com}
GATEWAY_IP_NAME=${GATEWAY_IP_NAME:-ember-gateway-ip}
GATEWAY_CERTIFICATE=${GATEWAY_CERTIFICATE:-ember-gateway-cert}
GATEWAY_SSL_POLICY=${GATEWAY_SSL_POLICY:-ember-gateway-modern}
GATEWAY_NAMESPACE=ember-system
GATEWAY_NAME=ember-public
GATEWAY_ROUTE_NAME=ember-public
GATEWAY_READY_TIMEOUT_MINUTES=${GATEWAY_READY_TIMEOUT_MINUTES:-60}
GATEWAY_DELETE_TIMEOUT_MINUTES=${GATEWAY_DELETE_TIMEOUT_MINUTES:-20}
CONFIRM_DESTROY=${CONFIRM_DESTROY:-}
GO_IMAGE=${GO_IMAGE:-golang:1.25@sha256:cbff9d1a9041b316010f2da6b701b6c0d597718cb90928c85eb597334a0d23d4}

GATEWAY_IP=
GATEWAY_READY_DEADLINE=0
ADDRESS_DESCRIPTION="Ember public Gateway address for ${GATEWAY_HOST}"
CERTIFICATE_DESCRIPTION="Ember public Gateway certificate for ${GATEWAY_HOST}"
SSL_POLICY_DESCRIPTION="Ember public Gateway TLS policy for ${GATEWAY_HOST}"

usage() {
  cat <<'EOF'
Usage: scripts/gke-public-gateway.sh <prepare|deploy|status|smoke|destroy>

  prepare  Reserve the global IP, managed certificate, and TLS policy, then print the DNS record.
  deploy   Verify DNS-only resolution, apply the GKE Gateway resources, and wait until HTTPS is ready.
  status   Show the GCP edge resources, DNS answers, and Kubernetes Gateway status.
  smoke    Verify that only authenticated /v1 traffic reaches Ember through the public edge.
  destroy  Remove the Gateway and its dedicated GCP resources after explicit confirmation.
EOF
}

die() {
  echo "error: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

validate_resource_name() {
  local label=$1
  local value=$2
  [[ "${value}" =~ ^[a-z]$|^[a-z][a-z0-9-]{0,61}[a-z0-9]$ ]] ||
    die "${label} must be a lowercase GCP resource name with at most 63 characters"
}

validate_kubernetes_name() {
  [[ "$2" =~ ^[a-z0-9]$|^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$ ]] ||
    die "$1 must be a valid Kubernetes DNS label"
}

validate_dns_name() {
  local hostname=$1
  local label
  local -a labels
  [[ "${#hostname}" -le 253 && "${hostname}" == *.* &&
    "${hostname}" != .* && "${hostname}" != *. && "${hostname}" != *..* ]] ||
    die "GATEWAY_HOST must be a fully qualified DNS name"
  IFS=. read -r -a labels <<<"${hostname}"
  for label in "${labels[@]}"; do
    [[ "${label}" =~ ^[a-z0-9]$|^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$ ]] ||
      die "GATEWAY_HOST must contain valid lowercase DNS labels"
  done
}

validate_positive_integer() {
  [[ "$2" =~ ^[1-9][0-9]*$ ]] || die "$1 must be a positive integer"
}

resolve_config() {
  require_command "${GCLOUD}"
  require_command jq

  if [[ -z "${PROJECT_ID}" ]]; then
    PROJECT_ID=$("${GCLOUD}" config get-value project 2>/dev/null || true)
  fi
  [[ -n "${PROJECT_ID}" && "${PROJECT_ID}" != "(unset)" ]] ||
    die "PROJECT_ID is required or gcloud must have a default project"

  [[ "${PROJECT_ID}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "invalid PROJECT_ID"
  [[ "${CLUSTER_NAME}" =~ ^[a-z][a-z0-9-]{0,39}$ ]] || die "invalid CLUSTER_NAME"
  [[ "${CLUSTER_LOCATION}" =~ ^[a-z]+-[a-z]+[0-9]+-[a-z]$ ]] || die "invalid CLUSTER_LOCATION"
  validate_resource_name "GATEWAY_IP_NAME" "${GATEWAY_IP_NAME}"
  validate_resource_name "GATEWAY_CERTIFICATE" "${GATEWAY_CERTIFICATE}"
  validate_resource_name "GATEWAY_SSL_POLICY" "${GATEWAY_SSL_POLICY}"
  validate_kubernetes_name "GATEWAY_NAMESPACE" "${GATEWAY_NAMESPACE}"
  validate_kubernetes_name "GATEWAY_NAME" "${GATEWAY_NAME}"
  validate_kubernetes_name "GATEWAY_ROUTE_NAME" "${GATEWAY_ROUTE_NAME}"
  validate_dns_name "${GATEWAY_HOST}"
  validate_positive_integer "GATEWAY_READY_TIMEOUT_MINUTES" "${GATEWAY_READY_TIMEOUT_MINUTES}"
  validate_positive_integer "GATEWAY_DELETE_TIMEOUT_MINUTES" "${GATEWAY_DELETE_TIMEOUT_MINUTES}"
}

cluster_exists() {
  local output
  if output=$("${GCLOUD}" container clusters describe "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" \
    --format=json 2>&1); then
    return 0
  fi
  if [[ "${output}" == *"was not found"* || "${output}" == *"NOT_FOUND"* || "${output}" == *"not found"* ]]; then
    return 1
  fi
  printf '%s\n' "${output}" >&2
  die "could not inspect cluster ${CLUSTER_NAME}"
}

get_cluster_credentials() {
  require_command "${KUBECTL}"
  require_command gke-gcloud-auth-plugin
  "${GCLOUD}" container clusters get-credentials "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" \
    --quiet >/dev/null
}

require_gateway_api() {
  local description channel http_load_balancing_disabled
  description=$("${GCLOUD}" container clusters describe "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" \
    --format=json)
  channel=$(jq -r '.networkConfig.gatewayApiConfig.channel // empty' <<<"${description}")
  http_load_balancing_disabled=$(jq -r '.addonsConfig.httpLoadBalancing.disabled // false' <<<"${description}")
  [[ "${channel}" == "CHANNEL_STANDARD" ]] ||
    die "GKE Gateway API standard channel is not ready; run make gke-cluster-create first"
  [[ "${http_load_balancing_disabled}" != "true" ]] ||
    die "the cluster has the HttpLoadBalancing add-on disabled"

  if ! "${KUBECTL}" wait \
    --for=condition=Accepted \
    gatewayclass/gke-l7-global-external-managed \
    --timeout=10m >/dev/null; then
    "${KUBECTL}" get gatewayclass gke-l7-global-external-managed -o yaml || true
    die "the global external managed GatewayClass is not accepted yet"
  fi
}

load_address() {
  local description
  description=$("${GCLOUD}" compute addresses describe "${GATEWAY_IP_NAME}" \
    --project="${PROJECT_ID}" \
    --global \
    --format=json) || die "global address ${GATEWAY_IP_NAME} does not exist; run prepare first"
  jq -e --arg expected_description "${ADDRESS_DESCRIPTION}" \
    '.addressType == "EXTERNAL" and .ipVersion == "IPV4" and .networkTier == "PREMIUM" and .description == $expected_description' \
    <<<"${description}" >/dev/null ||
    die "existing global address ${GATEWAY_IP_NAME} is not the dedicated Ember external premium IPv4 address"
  GATEWAY_IP=$(jq -r '.address // empty' <<<"${description}")
  [[ -n "${GATEWAY_IP}" ]] || die "global address ${GATEWAY_IP_NAME} has no assigned IP"
}

ensure_address() {
  if "${GCLOUD}" compute addresses describe "${GATEWAY_IP_NAME}" \
    --project="${PROJECT_ID}" \
    --global \
    --format=json >/dev/null 2>&1; then
    load_address
    return
  fi

  "${GCLOUD}" compute addresses create "${GATEWAY_IP_NAME}" \
    --project="${PROJECT_ID}" \
    --global \
    --ip-version=IPV4 \
    --network-tier=PREMIUM \
    --description="${ADDRESS_DESCRIPTION}" \
    --quiet
  load_address
}

validate_certificate() {
  jq -e \
    --arg host "${GATEWAY_HOST}" \
    --arg expected_description "${CERTIFICATE_DESCRIPTION}" \
    '.type == "MANAGED" and .description == $expected_description and .managed.domains == [$host]' \
    <<<"$1" >/dev/null ||
    die "existing certificate ${GATEWAY_CERTIFICATE} is not the dedicated managed certificate for ${GATEWAY_HOST}"
}

ensure_certificate() {
  local description
  if description=$("${GCLOUD}" compute ssl-certificates describe "${GATEWAY_CERTIFICATE}" \
    --project="${PROJECT_ID}" \
    --global \
    --format=json 2>/dev/null); then
    validate_certificate "${description}"
    return
  fi

  "${GCLOUD}" compute ssl-certificates create "${GATEWAY_CERTIFICATE}" \
    --project="${PROJECT_ID}" \
    --global \
    --domains="${GATEWAY_HOST}" \
    --description="${CERTIFICATE_DESCRIPTION}" \
    --quiet
}

validate_ssl_policy() {
  jq -e --arg expected_description "${SSL_POLICY_DESCRIPTION}" \
    '.profile == "MODERN" and .minTlsVersion == "TLS_1_2" and .description == $expected_description' \
    <<<"$1" >/dev/null ||
    die "existing SSL policy ${GATEWAY_SSL_POLICY} is not the dedicated Ember MODERN TLS 1.2 policy"
}

ensure_ssl_policy() {
  local description
  if description=$("${GCLOUD}" compute ssl-policies describe "${GATEWAY_SSL_POLICY}" \
    --project="${PROJECT_ID}" \
    --global \
    --format=json 2>/dev/null); then
    validate_ssl_policy "${description}"
    return
  fi

  "${GCLOUD}" compute ssl-policies create "${GATEWAY_SSL_POLICY}" \
    --project="${PROJECT_ID}" \
    --global \
    --profile=MODERN \
    --min-tls-version=1.2 \
    --description="${SSL_POLICY_DESCRIPTION}" \
    --quiet
}

ensure_cloud_resources() {
  "${GCLOUD}" services enable compute.googleapis.com container.googleapis.com \
    --project="${PROJECT_ID}" \
    --quiet
  ensure_address
  ensure_certificate
  ensure_ssl_policy
}

keep_cluster_for_public_gateway() {
  PROJECT_ID="${PROJECT_ID}" \
    CLUSTER_NAME="${CLUSTER_NAME}" \
    CLUSTER_LOCATION="${CLUSTER_LOCATION}" \
    ./scripts/gcp-cost-guard.sh keep-cluster
  echo "Any GPU-pool cleanup timer remains unchanged; the cluster timer is disabled while the public Gateway exists."
}

print_dns_instructions() {
  cat <<EOF

Cloudflare DNS record required before deployment:
  Type:  A
  Name:  ${GATEWAY_HOST}
  Value: ${GATEWAY_IP}
  Proxy: DNS only (gray cloud)
  TTL:   Auto

Do not create an AAAA record for this IPv4-only Gateway. After DNS resolves directly
to ${GATEWAY_IP}, run:
  make gke-gateway-deploy \\
    GCP_PROJECT=${PROJECT_ID} \\
    GATEWAY_HOST=${GATEWAY_HOST} \\
    GATEWAY_IP_NAME=${GATEWAY_IP_NAME} \\
    GATEWAY_CERTIFICATE=${GATEWAY_CERTIFICATE} \\
    GATEWAY_SSL_POLICY=${GATEWAY_SSL_POLICY}
EOF
}

prepare() {
  resolve_config
  require_command "${KUBECTL}"
  cluster_exists || die "cluster ${CLUSTER_NAME} does not exist in ${PROJECT_ID}/${CLUSTER_LOCATION}"
  get_cluster_credentials
  require_gateway_api
  ensure_cloud_resources
  print_dns_instructions
}

verify_dns() {
  local ipv4 ipv6
  require_command "${DIG}"
  ipv4=$("${DIG}" +short A "${GATEWAY_HOST}" | sed '/^$/d' | sort -u)
  [[ "${ipv4}" == "${GATEWAY_IP}" ]] ||
    die "${GATEWAY_HOST} must resolve only to ${GATEWAY_IP}; use a Cloudflare DNS-only A record and wait for propagation"
  ipv6=$("${DIG}" +short AAAA "${GATEWAY_HOST}" | sed '/^$/d' | sort -u)
  [[ -z "${ipv6}" ]] ||
    die "${GATEWAY_HOST} has an AAAA answer; remove it because this Gateway reserves only IPv4"
}

render_manifests() {
  local output=$1
  "${KUBECTL}" kustomize deploy/gke/public-gateway |
    sed \
      -e "s|EMBER_GATEWAY_HOST|${GATEWAY_HOST}|g" \
      -e "s|EMBER_GATEWAY_IP_NAME|${GATEWAY_IP_NAME}|g" \
      -e "s|EMBER_GATEWAY_CERTIFICATE|${GATEWAY_CERTIFICATE}|g" \
      -e "s|EMBER_GATEWAY_SSL_POLICY|${GATEWAY_SSL_POLICY}|g" >"${output}"
  if grep -q 'EMBER_GATEWAY_' "${output}"; then
    die "public Gateway manifest rendering left unresolved placeholders"
  fi
}

validate_backend() {
  local service
  service=$("${KUBECTL}" -n "${GATEWAY_NAMESPACE}" get service ember-gateway -o json) ||
    die "ember-gateway Service is not deployed; run make gke-deploy first"
  jq -e 'any(.spec.ports[]?; .port == 8080)' <<<"${service}" >/dev/null ||
    die "ember-gateway Service does not expose port 8080"
  "${KUBECTL}" -n "${GATEWAY_NAMESPACE}" wait \
    --for=condition=Available \
    deployment/ember-gateway \
    --timeout=5m >/dev/null ||
    die "ember-gateway Deployment is not available"
}

wait_for_kubernetes_condition() {
  local resource=$1
  local filter=$2
  local label=$3
  local description

  while ((SECONDS < GATEWAY_READY_DEADLINE)); do
    if description=$("${KUBECTL}" -n "${GATEWAY_NAMESPACE}" get "${resource}" -o json 2>/dev/null); then
      if jq -e "${filter}" <<<"${description}" >/dev/null; then
        echo "${label} is ready."
        return
      fi
    fi
    sleep 10
  done

  "${KUBECTL}" -n "${GATEWAY_NAMESPACE}" describe "${resource}" || true
  die "timed out waiting for ${label}"
}

wait_for_certificate() {
  local description status

  while ((SECONDS < GATEWAY_READY_DEADLINE)); do
    description=$("${GCLOUD}" compute ssl-certificates describe "${GATEWAY_CERTIFICATE}" \
      --project="${PROJECT_ID}" \
      --global \
      --format=json)
    status=$(jq -r '.managed.status // empty' <<<"${description}")
    case "${status}" in
      ACTIVE)
        echo "Google-managed certificate is active."
        return
        ;;
      *FAILED*)
        jq '.managed' <<<"${description}" >&2
        die "Google-managed certificate provisioning failed"
        ;;
    esac
    if jq -e 'any((.managed.domainStatus // {})[]; startswith("FAILED"))' \
      <<<"${description}" >/dev/null; then
      jq '.managed' <<<"${description}" >&2
      die "Google-managed certificate domain validation failed"
    fi
    sleep 15
  done

  "${GCLOUD}" compute ssl-certificates describe "${GATEWAY_CERTIFICATE}" \
    --project="${PROJECT_ID}" \
    --global \
    --format='yaml(managed)' >&2 || true
  die "timed out waiting for the Google-managed certificate; verify the DNS-only A record"
}

deploy() {
  local rendered gateway_address
  resolve_config
  require_command "${KUBECTL}"
  require_command sed
  require_command grep
  require_command mktemp
  cluster_exists || die "cluster ${CLUSTER_NAME} does not exist in ${PROJECT_ID}/${CLUSTER_LOCATION}"
  get_cluster_credentials
  require_gateway_api
  validate_backend
  ensure_cloud_resources
  verify_dns
  keep_cluster_for_public_gateway

  rendered=$(mktemp)
  trap 'rm -f "${rendered}"' EXIT
  render_manifests "${rendered}"
  "${KUBECTL}" apply -f "${rendered}"
  GATEWAY_READY_DEADLINE=$((SECONDS + GATEWAY_READY_TIMEOUT_MINUTES * 60))

  wait_for_kubernetes_condition \
    "healthcheckpolicy/ember-gateway" \
    '.metadata.generation as $generation | any(.status.conditions[]?; .observedGeneration == $generation and .type == "Attached" and .status == "True")' \
    "HealthCheckPolicy"
  wait_for_kubernetes_condition \
    "gcpbackendpolicy/ember-gateway" \
    '.metadata.generation as $generation | any(.status.conditions[]?; .observedGeneration == $generation and .type == "Attached" and .status == "True")' \
    "GCPBackendPolicy"
  wait_for_kubernetes_condition \
    "gcpgatewaypolicy/${GATEWAY_NAME}" \
    '.metadata.generation as $generation | any(.status.conditions[]?; .observedGeneration == $generation and .type == "Attached" and .status == "True")' \
    "GCPGatewayPolicy"
  wait_for_kubernetes_condition \
    "httproute/${GATEWAY_ROUTE_NAME}" \
    '.metadata.generation as $generation | (any(.status.parents[]?.conditions[]?; .observedGeneration == $generation and .type == "Accepted" and .status == "True")) and (any(.status.parents[]?.conditions[]?; .observedGeneration == $generation and (.type == "Reconciled" or .type == "ResolvedRefs") and .status == "True"))' \
    "HTTPRoute"
  wait_for_kubernetes_condition \
    "gateway/${GATEWAY_NAME}" \
    '.metadata.generation as $generation | any(.status.conditions[]?; .observedGeneration == $generation and (.type == "Programmed" or .type == "Ready") and .status == "True")' \
    "Gateway"
  wait_for_certificate

  gateway_address=$("${KUBECTL}" -n "${GATEWAY_NAMESPACE}" get "gateway/${GATEWAY_NAME}" \
    -o jsonpath='{.status.addresses[0].value}')
  [[ "${gateway_address}" == "${GATEWAY_IP}" ]] ||
    die "Gateway address ${gateway_address:-<empty>} does not match reserved IP ${GATEWAY_IP}"

  rm -f "${rendered}"
  trap - EXIT
  echo "Public Gateway ready: https://${GATEWAY_HOST}/v1"
  echo "Run make gke-gateway-smoke GCP_PROJECT=${PROJECT_ID} GATEWAY_HOST=${GATEWAY_HOST} to verify the edge policy."
}

show_cloud_status() {
  if resource_exists address; then
    "${GCLOUD}" compute addresses describe "${GATEWAY_IP_NAME}" \
      --project="${PROJECT_ID}" \
      --global \
      --format='table(name,address,status,networkTier)'
    load_address
  else
    echo "Global address ${GATEWAY_IP_NAME}: not found"
  fi

  if resource_exists certificate; then
    "${GCLOUD}" compute ssl-certificates describe "${GATEWAY_CERTIFICATE}" \
      --project="${PROJECT_ID}" \
      --global \
      --format='table(name,managed.status,managed.domainStatus)'
  else
    echo "Managed certificate ${GATEWAY_CERTIFICATE}: not found"
  fi
  if resource_exists ssl-policy; then
    "${GCLOUD}" compute ssl-policies describe "${GATEWAY_SSL_POLICY}" \
      --project="${PROJECT_ID}" \
      --global \
      --format='table(name,profile,minTlsVersion)'
  else
    echo "SSL policy ${GATEWAY_SSL_POLICY}: not found"
  fi
}

status() {
  resolve_config
  show_cloud_status

  if command -v "${DIG}" >/dev/null 2>&1; then
    echo
    echo "DNS A answers for ${GATEWAY_HOST}:"
    "${DIG}" +short A "${GATEWAY_HOST}" || true
    echo "DNS AAAA answers for ${GATEWAY_HOST}:"
    "${DIG}" +short AAAA "${GATEWAY_HOST}" || true
  fi

  if cluster_exists; then
    get_cluster_credentials
    echo
    "${KUBECTL}" -n "${GATEWAY_NAMESPACE}" get \
      "gateway/${GATEWAY_NAME}" \
      "httproute/${GATEWAY_ROUTE_NAME}" \
      "healthcheckpolicy/ember-gateway" \
      "gcpbackendpolicy/ember-gateway" \
      "gcpgatewaypolicy/${GATEWAY_NAME}" 2>/dev/null ||
      echo "One or more Kubernetes public Gateway resources are not deployed."
  else
    echo "Cluster ${CLUSTER_NAME}: not found"
  fi

  echo
  PROJECT_ID="${PROJECT_ID}" \
    CLUSTER_NAME="${CLUSTER_NAME}" \
    CLUSTER_LOCATION="${CLUSTER_LOCATION}" \
    ./scripts/gcp-cost-guard.sh status
}

request_status() {
  local path=$1
  local expected=$2
  local token=${3:-}
  local code
  local -a args=(
    --silent
    --show-error
    --connect-timeout 10
    --max-time 30
    --output "${SMOKE_BODY}"
    --write-out '%{http_code}'
  )
  if [[ -n "${token}" ]]; then
    args+=(--header "Authorization: Bearer ${token}")
  fi
  code=$("${CURL}" "${args[@]}" "https://${GATEWAY_HOST}${path}")
  [[ "${code}" == "${expected}" ]] ||
    die "GET ${path} returned HTTP ${code}, expected ${expected}; body: $(head -c 500 "${SMOKE_BODY}")"
}

smoke() {
  local token
  resolve_config
  require_command "${KUBECTL}"
  require_command "${CURL}"
  require_command "${DOCKER}"
  require_command mktemp
  load_address
  verify_dns
  cluster_exists || die "cluster ${CLUSTER_NAME} does not exist in ${PROJECT_ID}/${CLUSTER_LOCATION}"
  get_cluster_credentials

  SMOKE_BODY=$(mktemp)
  trap 'rm -f "${SMOKE_BODY}"' EXIT

  request_status "/healthz" "404"
  request_status "/metrics" "404"
  request_status "/v1/endpoints/ep-public-smoke" "401"
  jq -e '.error.code == "unauthorized"' "${SMOKE_BODY}" >/dev/null ||
    die "unauthenticated /v1 response did not contain the expected unauthorized error"

  token=$("${KUBECTL}" -n "${GATEWAY_NAMESPACE}" get secret ember-jwt-keys \
    -o jsonpath='{.data.private\.key}' |
    "${DOCKER}" run --rm -i \
      --user "$(id -u):$(id -g)" \
      -e GOMODCACHE=/workspace/.cache/gomod \
      -e GOCACHE=/workspace/.cache/gocache \
      -v "${REPO_ROOT}:/workspace" \
      -w /workspace \
      "${GO_IMAGE}" \
      go run ./cmd/auth-tool token \
      --private-key-base64-stdin \
      --subject public_gateway_smoke \
      --ttl 120s)
  [[ -n "${token}" ]] || die "failed to issue a public Gateway smoke token"

  request_status "/v1/endpoints/ep-public-smoke" "404" "${token}"
  unset token
  jq -e '.error.code == "not_found"' "${SMOKE_BODY}" >/dev/null ||
    die "authenticated /v1 response did not reach the Ember Gateway"

  rm -f "${SMOKE_BODY}"
  trap - EXIT
  echo "Public Gateway smoke passed: TLS, route isolation, JWT enforcement, and backend reachability."
}

resource_exists() {
  local resource=$1
  local output
  case "$1" in
    certificate)
      if output=$("${GCLOUD}" compute ssl-certificates describe "${GATEWAY_CERTIFICATE}" \
        --project="${PROJECT_ID}" --global --format=json 2>&1); then
        return 0
      fi
      ;;
    ssl-policy)
      if output=$("${GCLOUD}" compute ssl-policies describe "${GATEWAY_SSL_POLICY}" \
        --project="${PROJECT_ID}" --global --format=json 2>&1); then
        return 0
      fi
      ;;
    address)
      if output=$("${GCLOUD}" compute addresses describe "${GATEWAY_IP_NAME}" \
        --project="${PROJECT_ID}" --global --format=json 2>&1); then
        return 0
      fi
      ;;
    *)
      return 1
      ;;
  esac
  if [[ "${output}" == *"was not found"* || "${output}" == *"NOT_FOUND"* || "${output}" == *"not found"* ]]; then
    return 1
  fi
  printf '%s\n' "${output}" >&2
  die "could not inspect ${resource}"
}

delete_resource_once() {
  case "$1" in
    certificate)
      "${GCLOUD}" compute ssl-certificates delete "${GATEWAY_CERTIFICATE}" \
        --project="${PROJECT_ID}" --global --quiet
      ;;
    ssl-policy)
      "${GCLOUD}" compute ssl-policies delete "${GATEWAY_SSL_POLICY}" \
        --project="${PROJECT_ID}" --global --quiet
      ;;
    address)
      "${GCLOUD}" compute addresses delete "${GATEWAY_IP_NAME}" \
        --project="${PROJECT_ID}" --global --quiet
      ;;
  esac
}

delete_cloud_resource() {
  local resource=$1
  local deadline=$((SECONDS + GATEWAY_DELETE_TIMEOUT_MINUTES * 60))
  local output
  while resource_exists "${resource}"; do
    if ((SECONDS >= deadline)); then
      die "timed out deleting ${resource}; the load balancer may still be releasing it"
    fi
    if output=$(delete_resource_once "${resource}" 2>&1); then
      sleep 5
      continue
    fi
    if [[ "${output}" == *"was not found"* || "${output}" == *"NOT_FOUND"* || "${output}" == *"not found"* ]]; then
      continue
    fi
    if [[ "${output}" != *"being used"* &&
      "${output}" != *"in use"* &&
      "${output}" != *"resourceInUseByAnotherResource"* ]]; then
      printf '%s\n' "${output}" >&2
      die "failed to delete ${resource}"
    fi
    sleep 15
  done
  echo "Deleted ${resource}."
}

validate_cloud_resources_for_destroy() {
  local description
  if resource_exists address; then
    load_address
  fi
  if resource_exists certificate; then
    description=$("${GCLOUD}" compute ssl-certificates describe "${GATEWAY_CERTIFICATE}" \
      --project="${PROJECT_ID}" --global --format=json)
    validate_certificate "${description}"
  fi
  if resource_exists ssl-policy; then
    description=$("${GCLOUD}" compute ssl-policies describe "${GATEWAY_SSL_POLICY}" \
      --project="${PROJECT_ID}" --global --format=json)
    validate_ssl_policy "${description}"
  fi
}

destroy() {
  local expected_confirmation
  resolve_config
  expected_confirmation="${PROJECT_ID}/${GATEWAY_HOST}"
  [[ "${CONFIRM_DESTROY}" == "${expected_confirmation}" ]] ||
    die "set CONFIRM_DESTROY=${expected_confirmation} to delete the public Gateway resources"
  validate_cloud_resources_for_destroy

  if cluster_exists; then
    get_cluster_credentials
    "${KUBECTL}" -n "${GATEWAY_NAMESPACE}" delete \
      "httproute/${GATEWAY_ROUTE_NAME}" \
      "healthcheckpolicy/ember-gateway" \
      "gcpbackendpolicy/ember-gateway" \
      "gcpgatewaypolicy/${GATEWAY_NAME}" \
      --ignore-not-found \
      --wait=true \
      --timeout="${GATEWAY_DELETE_TIMEOUT_MINUTES}m"
    "${KUBECTL}" -n "${GATEWAY_NAMESPACE}" delete \
      "gateway/${GATEWAY_NAME}" \
      --ignore-not-found \
      --wait=true \
      --timeout="${GATEWAY_DELETE_TIMEOUT_MINUTES}m"
  else
    echo "Cluster ${CLUSTER_NAME} is absent; continuing with dedicated GCP resource cleanup."
  fi

  delete_cloud_resource certificate
  delete_cloud_resource ssl-policy
  delete_cloud_resource address
  echo "Public Gateway resources removed. Delete the Cloudflare A record for ${GATEWAY_HOST}."
  echo "Review make gcp-cost-guard-status; re-arm timers only if the GPU pool still exists, or destroy the cluster explicitly."
}

case "${1:-}" in
  prepare)
    prepare
    ;;
  deploy)
    deploy
    ;;
  status)
    status
    ;;
  smoke)
    smoke
    ;;
  destroy)
    destroy
    ;;
  *)
    usage
    exit 1
    ;;
esac
