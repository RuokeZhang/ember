#!/usr/bin/env bash
set -euo pipefail

GCLOUD=${GCLOUD:-gcloud}
PROJECT_ID=${PROJECT_ID:-}
CLUSTER_NAME=${CLUSTER_NAME:-ember-gpu}
CLUSTER_LOCATION=${CLUSTER_LOCATION:-us-central1-a}
GPU_NODE_POOL=${GPU_NODE_POOL:-l4-spot}
CPU_MACHINE_TYPE=${CPU_MACHINE_TYPE:-e2-standard-4}
GPU_MACHINE_TYPE=${GPU_MACHINE_TYPE:-g2-standard-8}
NODE_SERVICE_ACCOUNT_NAME=${NODE_SERVICE_ACCOUNT_NAME:-ember-gke-nodes}
GPU_TTL_HOURS=${GPU_TTL_HOURS:-3}
CLUSTER_TTL_HOURS=${CLUSTER_TTL_HOURS:-6}
NODE_POOL_CREATE_TIMEOUT_MINUTES=${NODE_POOL_CREATE_TIMEOUT_MINUTES:-60}

usage() {
  cat <<'EOF'
Usage: scripts/gke-cluster.sh <command>

Commands:
  create       Create the CPU cluster, arm cleanup, then create one Spot L4 pool.
  credentials  Refresh kubectl credentials for the configured cluster.
  status       Show the cluster, node pools, nodes, and active cleanup tasks.

Environment:
  PROJECT_ID          Defaults to the active gcloud project.
  CLUSTER_NAME        Default: ember-gpu
  CLUSTER_LOCATION    Default: us-central1-a
  GPU_NODE_POOL       Default: l4-spot
  CPU_MACHINE_TYPE    Default: e2-standard-4
  GPU_MACHINE_TYPE    Default: g2-standard-8
  NODE_SERVICE_ACCOUNT_NAME
                      Default: ember-gke-nodes
  GPU_TTL_HOURS       Default: 3
  CLUSTER_TTL_HOURS   Default: 6
  NODE_POOL_CREATE_TIMEOUT_MINUTES
                      Default: 60
EOF
}

die() {
  echo "gke cluster: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

resolve_config() {
  require_command "${GCLOUD}"
  require_command kubectl
  require_command gke-gcloud-auth-plugin
  require_command jq
  if [[ -z "${PROJECT_ID}" ]]; then
    PROJECT_ID=$("${GCLOUD}" config get-value project 2>/dev/null || true)
  fi
  [[ "${PROJECT_ID}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "invalid PROJECT_ID"
  [[ "${CLUSTER_NAME}" =~ ^[a-z][a-z0-9-]{0,39}$ ]] || die "invalid CLUSTER_NAME"
  [[ "${GPU_NODE_POOL}" =~ ^[a-z][a-z0-9-]{0,39}$ ]] || die "invalid GPU_NODE_POOL"
  [[ "${NODE_SERVICE_ACCOUNT_NAME}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "invalid NODE_SERVICE_ACCOUNT_NAME"
  [[ "${CLUSTER_LOCATION}" =~ ^[a-z]+-[a-z]+[0-9]+-[a-z]$ ]] || die "CLUSTER_LOCATION must be a zone such as us-central1-a"
  [[ "${GPU_TTL_HOURS}" =~ ^[1-9][0-9]*$ ]] || die "GPU_TTL_HOURS must be a positive integer"
  [[ "${CLUSTER_TTL_HOURS}" =~ ^[1-9][0-9]*$ ]] || die "CLUSTER_TTL_HOURS must be a positive integer"
  [[ "${NODE_POOL_CREATE_TIMEOUT_MINUTES}" =~ ^[1-9][0-9]*$ ]] || die "NODE_POOL_CREATE_TIMEOUT_MINUTES must be a positive integer"
  ((CLUSTER_TTL_HOURS > GPU_TTL_HOURS)) || die "CLUSTER_TTL_HOURS must exceed GPU_TTL_HOURS"
  NODE_SERVICE_ACCOUNT_EMAIL="${NODE_SERVICE_ACCOUNT_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
}

cluster_exists() {
  "${GCLOUD}" container clusters describe "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" >/dev/null 2>&1
}

gpu_pool_exists() {
  "${GCLOUD}" container node-pools describe "${GPU_NODE_POOL}" \
    --cluster="${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" >/dev/null 2>&1
}

validate_existing_cluster() {
  local description datapath workload_pool http_load_balancing_disabled
  description=$("${GCLOUD}" container clusters describe "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" \
    --format=json)
  datapath=$(jq -r '.networkConfig.datapathProvider // empty' <<<"${description}")
  workload_pool=$(jq -r '.workloadIdentityConfig.workloadPool // empty' <<<"${description}")
  http_load_balancing_disabled=$(jq -r '.addonsConfig.httpLoadBalancing.disabled // false' <<<"${description}")
  [[ "${datapath}" == "ADVANCED_DATAPATH" ]] || die "existing cluster does not use GKE Dataplane V2"
  [[ "${workload_pool}" == "${PROJECT_ID}.svc.id.goog" ]] || die "existing cluster has an unexpected Workload Identity pool"
  [[ "${http_load_balancing_disabled}" != "true" ]] || die "existing cluster has the HttpLoadBalancing add-on disabled"
}

ensure_gateway_api() {
  local description channel
  description=$("${GCLOUD}" container clusters describe "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" \
    --format=json)
  channel=$(jq -r '.networkConfig.gatewayApiConfig.channel // empty' <<<"${description}")
  if [[ "${channel}" != "CHANNEL_STANDARD" ]]; then
    echo "Enabling the GKE Gateway API standard channel; this can take up to 45 minutes."
    "${GCLOUD}" container clusters update "${CLUSTER_NAME}" \
      --project="${PROJECT_ID}" \
      --location="${CLUSTER_LOCATION}" \
      --gateway-api=standard \
      --quiet
    description=$("${GCLOUD}" container clusters describe "${CLUSTER_NAME}" \
      --project="${PROJECT_ID}" \
      --location="${CLUSTER_LOCATION}" \
      --format=json)
    channel=$(jq -r '.networkConfig.gatewayApiConfig.channel // empty' <<<"${description}")
  fi
  [[ "${channel}" == "CHANNEL_STANDARD" ]] || die "GKE Gateway API standard channel is not enabled"
}

validate_existing_gpu_pool() {
  local description machine_type accelerator spot autoscaling_enabled min_nodes max_nodes label has_storage_scope
  if ! description=$("${GCLOUD}" container node-pools describe "${GPU_NODE_POOL}" \
    --cluster="${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" \
    --format=json); then
    echo "gke cluster: could not describe GPU node pool ${GPU_NODE_POOL}" >&2
    return 1
  fi
  machine_type=$(jq -r '.config.machineType // empty' <<<"${description}")
  accelerator=$(jq -r '.config.accelerators[0].acceleratorType // empty' <<<"${description}")
  spot=$(jq -r '.config.spot // false' <<<"${description}")
  autoscaling_enabled=$(jq -r '.autoscaling.enabled // false' <<<"${description}")
  min_nodes=$(jq -r '.autoscaling.minNodeCount // 0' <<<"${description}")
  max_nodes=$(jq -r '.autoscaling.maxNodeCount // 0' <<<"${description}")
  label=$(jq -r '.config.labels["ember.dev/gpu"] // empty' <<<"${description}")
  has_storage_scope=$(jq -r 'any(.config.oauthScopes[]?; . == "https://www.googleapis.com/auth/devstorage.read_only" or . == "https://www.googleapis.com/auth/cloud-platform")' <<<"${description}")
  if [[ "${machine_type}" != "${GPU_MACHINE_TYPE}" ]]; then
    echo "gke cluster: existing GPU pool uses ${machine_type}, expected ${GPU_MACHINE_TYPE}" >&2
    return 1
  fi
  if [[ "${accelerator}" != "nvidia-l4" ]]; then
    echo "gke cluster: existing GPU pool does not use NVIDIA L4" >&2
    return 1
  fi
  if [[ "${spot}" != "true" ]]; then
    echo "gke cluster: existing GPU pool is not Spot" >&2
    return 1
  fi
  if [[ "${autoscaling_enabled}" != "true" || "${min_nodes}" != "0" || "${max_nodes}" != "1" ]]; then
    echo "gke cluster: existing GPU pool autoscaling must be enabled with min=0 and max=1" >&2
    return 1
  fi
  if [[ "${label}" != "l4" ]]; then
    echo "gke cluster: existing GPU pool lacks ember.dev/gpu=l4" >&2
    return 1
  fi
  if [[ "${has_storage_scope}" != "true" ]]; then
    echo "gke cluster: existing GPU pool cannot download the managed NVIDIA driver" >&2
    return 1
  fi
}

cost_guard() {
  PROJECT_ID="${PROJECT_ID}" \
    CLUSTER_NAME="${CLUSTER_NAME}" \
    CLUSTER_LOCATION="${CLUSTER_LOCATION}" \
    GPU_NODE_POOL="${GPU_NODE_POOL}" \
    GPU_TTL_HOURS="${GPU_TTL_HOURS}" \
    CLUSTER_TTL_HOURS="${CLUSTER_TTL_HOURS}" \
    ALLOW_MISSING_GPU_POOL="${ALLOW_MISSING_GPU_POOL:-false}" \
    ./scripts/gcp-cost-guard.sh "$@"
}

preserve_cluster_if_public_gateway_exists() {
  local gateways
  if ! gateways=$(kubectl get gateways.gateway.networking.k8s.io -A -o json 2>/dev/null); then
    return
  fi
  if jq -e 'any(.items[]?; .spec.gatewayClassName == "gke-l7-global-external-managed")' \
    <<<"${gateways}" >/dev/null; then
    cost_guard keep-cluster
    echo "Public Gateway detected; retained the GPU-pool timer but removed the cluster deletion timer."
  fi
}

setup_node_service_account() {
  if ! "${GCLOUD}" iam service-accounts describe "${NODE_SERVICE_ACCOUNT_EMAIL}" \
    --project="${PROJECT_ID}" >/dev/null 2>&1; then
    "${GCLOUD}" iam service-accounts create "${NODE_SERVICE_ACCOUNT_NAME}" \
      --project="${PROJECT_ID}" \
      --display-name="Ember GKE nodes"
  fi
  for role in roles/container.defaultNodeServiceAccount roles/artifactregistry.reader; do
    "${GCLOUD}" projects add-iam-policy-binding "${PROJECT_ID}" \
      --member="serviceAccount:${NODE_SERVICE_ACCOUNT_EMAIL}" \
      --role="${role}" \
      --condition=None \
      --quiet >/dev/null
  done
}

get_credentials() {
  resolve_config
  cluster_exists || die "cluster ${CLUSTER_NAME} does not exist"
  "${GCLOUD}" container clusters get-credentials "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}"
}

wait_for_container_operation() {
  local operation_name=$1
  local deadline=$((SECONDS + NODE_POOL_CREATE_TIMEOUT_MINUTES * 60))
  local description status error_code error_message

  while ((SECONDS < deadline)); do
    if ! description=$("${GCLOUD}" container operations describe "${operation_name}" \
      --project="${PROJECT_ID}" \
      --location="${CLUSTER_LOCATION}" \
      --format=json); then
      sleep 10
      continue
    fi
    status=$(jq -r '.status // empty' <<<"${description}")
    case "${status}" in
    DONE)
      error_code=$(jq -r '.error.code // 0' <<<"${description}")
      if [[ ! "${error_code}" =~ ^[0-9]+$ ]]; then
        echo "GKE operation ${operation_name} returned invalid error code ${error_code}" >&2
        return 1
      fi
      if ((error_code != 0)); then
        error_message=$(jq -r '
          if ((.error.message // "") | length) > 0 then .error.message
          elif ((.statusMessage // "") | length) > 0 then .statusMessage
          else "unknown error"
          end
        ' <<<"${description}")
        echo "GKE operation ${operation_name} failed with code ${error_code}: ${error_message}" >&2
        return 1
      fi
      return 0
      ;;
    PENDING | RUNNING | ABORTING)
      sleep 10
      ;;
    *)
      echo "GKE operation ${operation_name} reported unexpected status ${status}" >&2
      return 1
      ;;
    esac
  done

  echo "GKE operation ${operation_name} did not finish within ${NODE_POOL_CREATE_TIMEOUT_MINUTES} minutes" >&2
  return 1
}

delete_new_cluster() {
  echo "Deleting the newly created cluster after setup failure." >&2
  if "${GCLOUD}" container clusters delete "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" \
    --quiet; then
    cost_guard disarm
  else
    echo "Cluster deletion failed; leaving the armed cleanup tasks in place." >&2
    return 1
  fi
}

create_cluster() {
  resolve_config
  "${GCLOUD}" services enable container.googleapis.com compute.googleapis.com \
    --project="${PROJECT_ID}" \
    --quiet
  cost_guard setup
  setup_node_service_account

  local created_cluster=false
  if cluster_exists; then
    validate_existing_cluster
    echo "Cluster ${CLUSTER_NAME} already exists; preserving it."
  else
    "${GCLOUD}" container clusters create "${CLUSTER_NAME}" \
      --project="${PROJECT_ID}" \
      --location="${CLUSTER_LOCATION}" \
      --release-channel=regular \
      --machine-type="${CPU_MACHINE_TYPE}" \
      --num-nodes=1 \
      --disk-type=pd-balanced \
      --disk-size=50 \
      --image-type=COS_CONTAINERD \
      --enable-ip-alias \
      --enable-dataplane-v2 \
      --gateway-api=standard \
      --enable-shielded-nodes \
      --workload-pool="${PROJECT_ID}.svc.id.goog" \
      --service-account="${NODE_SERVICE_ACCOUNT_EMAIL}" \
      --scopes=gke-default \
      --metadata=disable-legacy-endpoints=true \
      --logging=SYSTEM \
      --monitoring=SYSTEM
    created_cluster=true
  fi

  ensure_gateway_api

  if ! "${GCLOUD}" container clusters get-credentials "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}"; then
    if [[ "${created_cluster}" == "true" ]]; then
      delete_new_cluster
    fi
    die "could not configure kubectl credentials"
  fi

  if ! ALLOW_MISSING_GPU_POOL=true cost_guard arm; then
    if [[ "${created_cluster}" == "true" ]]; then
      delete_new_cluster
    fi
    die "cleanup timers could not be armed; GPU creation was blocked"
  fi
  preserve_cluster_if_public_gateway_exists

  if gpu_pool_exists; then
    validate_existing_gpu_pool || die "existing GPU node pool failed validation"
    echo "GPU node pool ${GPU_NODE_POOL} already exists; cleanup timers were refreshed."
    return
  fi

  local operation_name
  if ! operation_name=$("${GCLOUD}" container node-pools create "${GPU_NODE_POOL}" \
    --cluster="${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" \
    --machine-type="${GPU_MACHINE_TYPE}" \
    --accelerator=type=nvidia-l4,count=1,gpu-driver-version=latest \
    --spot \
    --num-nodes=1 \
    --enable-autoscaling \
    --min-nodes=0 \
    --max-nodes=1 \
    --disk-type=pd-balanced \
    --disk-size=100 \
    --image-type=COS_CONTAINERD \
    --node-labels=ember.dev/gpu=l4 \
    --node-taints=nvidia.com/gpu=present:NoSchedule \
    --service-account="${NODE_SERVICE_ACCOUNT_EMAIL}" \
    --scopes=gke-default \
    --metadata=disable-legacy-endpoints=true \
    --enable-autorepair \
    --enable-autoupgrade \
    --async \
    --format='value(name)'); then
    if [[ "${created_cluster}" == "true" ]]; then
      delete_new_cluster
    fi
    die "GPU node pool creation failed"
  fi
  operation_name=${operation_name##*/}
  [[ "${operation_name}" =~ ^operation-[a-zA-Z0-9-]+$ ]] || {
    if [[ "${created_cluster}" == "true" ]]; then
      delete_new_cluster
    fi
    die "GPU node pool creation did not return an operation name"
  }
  if ! wait_for_container_operation "${operation_name}" || ! gpu_pool_exists; then
    if [[ "${created_cluster}" == "true" ]]; then
      delete_new_cluster
    fi
    die "GPU node pool creation failed"
  fi
  if ! validate_existing_gpu_pool; then
    if [[ "${created_cluster}" == "true" ]]; then
      delete_new_cluster
    fi
    die "GPU node pool failed post-create validation"
  fi

  echo "Created one Spot NVIDIA L4 node with active ${GPU_TTL_HOURS}h/${CLUSTER_TTL_HOURS}h cleanup timers."
}

show_status() {
  resolve_config
  if ! cluster_exists; then
    echo "Cluster ${CLUSTER_NAME} does not exist."
    cost_guard status
    return
  fi
  "${GCLOUD}" container clusters describe "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" \
    --format='table(name,status,location,currentMasterVersion)'
  "${GCLOUD}" container node-pools list \
    --cluster="${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" \
    --format='table(name,status,config.machineType,autoscaling.enabled,autoscaling.minNodeCount,autoscaling.maxNodeCount)'
  "${GCLOUD}" container clusters get-credentials "${CLUSTER_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${CLUSTER_LOCATION}" >/dev/null
  kubectl get nodes -L ember.dev/gpu -o wide
  cost_guard status
}

case "${1:-}" in
create)
  create_cluster
  ;;
credentials)
  get_credentials
  ;;
status)
  show_status
  ;;
-h | --help | help | "")
  usage
  ;;
*)
  usage >&2
  exit 1
  ;;
esac
