#!/usr/bin/env bash
set -euo pipefail

GCLOUD=${GCLOUD:-gcloud}
KUBECTL=${KUBECTL:-kubectl}
PROJECT_ID=${PROJECT_ID:-}
CLUSTER_NAME=${CLUSTER_NAME:-ember-gpu}
CLUSTER_LOCATION=${CLUSTER_LOCATION:-us-central1-a}
REGION=${REGION:-us-central1}
REPOSITORY=${REPOSITORY:-ember}
IMAGE_TAG=${IMAGE_TAG:-}

usage() {
  cat <<'EOF'
Usage: scripts/gke-deploy.sh

Deploy the real-runtime GKE overlay using Artifact Registry images resolved to digests.
EOF
}

die() {
  echo "gke deploy: $*" >&2
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

resolve_image() {
  local image_name=$1
  local tagged_image="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPOSITORY}/${image_name}:${IMAGE_TAG}"
  local description digest qualified
  description=$("${GCLOUD}" artifacts docker images describe "${tagged_image}" \
    --project="${PROJECT_ID}" \
    --format=json)
  digest=$(jq -r '.image_summary.digest // empty' <<<"${description}")
  if [[ -z "${digest}" ]]; then
    qualified=$(jq -r '.image_summary.fully_qualified_digest // empty' <<<"${description}")
    digest=${qualified##*@}
  fi
  [[ "${digest}" =~ ^sha256:[a-f0-9]{64}$ ]] || die "could not resolve digest for ${tagged_image}"
  printf '%s@%s' "${tagged_image%:*}" "${digest}"
}

for command in "${GCLOUD}" "${KUBECTL}" gke-gcloud-auth-plugin jq git make sed; do
  require_command "${command}"
done

if [[ -z "${PROJECT_ID}" ]]; then
  PROJECT_ID=$("${GCLOUD}" config get-value project 2>/dev/null || true)
fi
if [[ -z "${IMAGE_TAG}" ]]; then
  IMAGE_TAG=$(git rev-parse --short=12 HEAD)
fi
[[ "${PROJECT_ID}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "invalid PROJECT_ID"
[[ "${CLUSTER_LOCATION}" =~ ^[a-z]+-[a-z]+[0-9]+-[a-z]$ ]] || die "invalid CLUSTER_LOCATION"
[[ "${REGION}" =~ ^[a-z]+-[a-z]+[0-9]+$ ]] || die "invalid REGION"
[[ "${IMAGE_TAG}" =~ ^[A-Za-z0-9._-]{1,128}$ ]] || die "invalid IMAGE_TAG"

"${GCLOUD}" container clusters get-credentials "${CLUSTER_NAME}" \
  --project="${PROJECT_ID}" \
  --location="${CLUSTER_LOCATION}"

dns_service_ip=$("${KUBECTL}" -n kube-system get service kube-dns -o jsonpath='{.spec.clusterIP}')
[[ "${dns_service_ip}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || die "could not resolve the cluster DNS Service IPv4 address"
dns_service_cidr="${dns_service_ip}/32"

operator_image=$(resolve_image ember-operator)
prefetch_image=$(resolve_image ember-prefetch)
gateway_image=$(resolve_image ember-gateway)
control_api_image=$(resolve_image ember-control-api)

rendered=$(mktemp)
cleanup() {
  rm -f "${rendered}"
}
trap cleanup EXIT

"${KUBECTL}" kustomize deploy/gke |
  sed \
    -e "s|image: ember-operator:dev|image: ${operator_image}|g" \
    -e "s|image: ember-gateway:dev|image: ${gateway_image}|g" \
    -e "s|image: ember-control-api:dev|image: ${control_api_image}|g" \
    -e "s|--prefetch-image=EMBER_PREFETCH_IMAGE|--prefetch-image=${prefetch_image}|g" \
    -e "s|GKE_DNS_CIDR|${dns_service_cidr}|g" >"${rendered}"

if grep -Eq 'image: ember-(operator|gateway|control-api):dev|EMBER_PREFETCH_IMAGE|GKE_DNS_CIDR' "${rendered}"; then
  die "rendered manifest still contains local image placeholders"
fi

"${KUBECTL}" apply -f deploy/gke/namespace.yaml
make cluster-auth
./scripts/install-keda.sh
postgres_cluster_ip=$("${KUBECTL}" -n ember-system get service ember-postgres -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
if [[ -n "${postgres_cluster_ip}" && "${postgres_cluster_ip}" != "None" ]]; then
  echo "Recreating ember-postgres as a headless Service."
  "${KUBECTL}" -n ember-system delete service ember-postgres --wait=true
fi
"${KUBECTL}" apply -f "${rendered}"

"${KUBECTL}" -n ember-system rollout status statefulset/ember-postgres --timeout=5m
"${KUBECTL}" -n ember-system rollout status deployment/ember-prometheus --timeout=5m
"${KUBECTL}" -n ember-system rollout status deployment/ember-gateway --timeout=5m
"${KUBECTL}" -n ember-system rollout status deployment/ember-control-api --timeout=5m
"${KUBECTL}" -n ember-system rollout status deployment/ember-operator-controller-manager --timeout=5m

manager_args=$("${KUBECTL}" -n ember-system get deployment ember-operator-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}')
[[ "${manager_args}" != *"--simulation-mode"* ]] || die "GKE manager unexpectedly enabled simulation mode"
[[ "${manager_args}" == *"${prefetch_image}"* ]] || die "GKE manager did not receive the digest-pinned prefetch image"

echo "Deployed Ember real-runtime control plane with digest-pinned repository images."
