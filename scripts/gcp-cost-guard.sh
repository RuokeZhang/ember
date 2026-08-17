#!/usr/bin/env bash
set -euo pipefail

GCLOUD=${GCLOUD:-gcloud}
PROJECT_ID=${PROJECT_ID:-}
PROJECT_NUMBER=${PROJECT_NUMBER:-}
CLUSTER_NAME=${CLUSTER_NAME:-ember-gpu}
CLUSTER_LOCATION=${CLUSTER_LOCATION:-us-central1-a}
GPU_NODE_POOL=${GPU_NODE_POOL:-l4-spot}
GPU_TTL_HOURS=${GPU_TTL_HOURS:-3}
CLUSTER_TTL_HOURS=${CLUSTER_TTL_HOURS:-6}
TASKS_LOCATION=${TASKS_LOCATION:-}
QUEUE_NAME=${QUEUE_NAME:-ember-cost-guard}
SERVICE_ACCOUNT_NAME=${SERVICE_ACCOUNT_NAME:-ember-cost-guard}
ROLE_ID=${ROLE_ID:-emberCostGuard}
DRY_RUN=${DRY_RUN:-false}

usage() {
  cat <<'EOF'
Usage: scripts/gcp-cost-guard.sh <command>

Commands:
  setup         Create the queue, service account, and least-privilege IAM role.
  arm           Delete prior timers and schedule GPU-pool and cluster deletion.
  status        Show active deletion tasks for the configured cluster.
  disarm        Delete active deletion tasks without deleting GKE resources.
  destroy-gpu   Delete the GPU node pool now; requires CONFIRM_DESTROY.
  destroy-all   Delete the GKE cluster now; requires CONFIRM_DESTROY.

Environment:
  PROJECT_ID          Defaults to the active gcloud project.
  CLUSTER_NAME        Default: ember-gpu
  CLUSTER_LOCATION    Default: us-central1-a
  GPU_NODE_POOL       Default: l4-spot
  GPU_TTL_HOURS       Default: 3
  CLUSTER_TTL_HOURS   Default: 6
  TASKS_LOCATION      Defaults to the cluster's region.
  DRY_RUN             Set to true to print mutations without running them.
EOF
}

die() {
  echo "cost guard: $*" >&2
  exit 1
}

is_dry_run() {
  [[ "${DRY_RUN}" == "true" || "${DRY_RUN}" == "1" ]]
}

print_command() {
  printf '  '
  printf '%q ' "$@"
  printf '\n'
}

run() {
  if is_dry_run; then
    print_command "$@"
    return
  fi
  "$@"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

validate_positive_integer() {
  [[ "$2" =~ ^[1-9][0-9]*$ ]] || die "$1 must be a positive integer"
}

resolve_config() {
  require_command "${GCLOUD}"

  if [[ -z "${PROJECT_ID}" ]]; then
    PROJECT_ID=$("${GCLOUD}" config get-value project 2>/dev/null || true)
  fi
  [[ -n "${PROJECT_ID}" && "${PROJECT_ID}" != "(unset)" ]] || die "PROJECT_ID is required"
  [[ "${PROJECT_ID}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "invalid PROJECT_ID"
  [[ "${CLUSTER_NAME}" =~ ^[a-z][a-z0-9-]{0,39}$ ]] || die "invalid CLUSTER_NAME"
  [[ "${GPU_NODE_POOL}" =~ ^[a-z][a-z0-9-]{0,39}$ ]] || die "invalid GPU_NODE_POOL"
  [[ "${CLUSTER_LOCATION}" =~ ^[a-z]+-[a-z]+[0-9]+(-[a-z])?$ ]] || die "invalid CLUSTER_LOCATION"
  [[ "${QUEUE_NAME}" =~ ^[a-z][a-z0-9-]{0,99}$ ]] || die "invalid QUEUE_NAME"
  [[ "${SERVICE_ACCOUNT_NAME}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "invalid SERVICE_ACCOUNT_NAME"
  [[ "${ROLE_ID}" =~ ^[A-Za-z0-9_]{3,64}$ ]] || die "invalid ROLE_ID"
  validate_positive_integer GPU_TTL_HOURS "${GPU_TTL_HOURS}"
  validate_positive_integer CLUSTER_TTL_HOURS "${CLUSTER_TTL_HOURS}"
  ((CLUSTER_TTL_HOURS > GPU_TTL_HOURS)) || die "CLUSTER_TTL_HOURS must exceed GPU_TTL_HOURS"

  if [[ -z "${TASKS_LOCATION}" ]]; then
    if [[ "${CLUSTER_LOCATION}" =~ -[a-z]$ ]]; then
      TASKS_LOCATION=${CLUSTER_LOCATION%-?}
    else
      TASKS_LOCATION=${CLUSTER_LOCATION}
    fi
  fi

  if [[ -z "${PROJECT_NUMBER}" ]]; then
    PROJECT_NUMBER=$("${GCLOUD}" projects describe "${PROJECT_ID}" --format='value(projectNumber)')
  fi
  [[ "${PROJECT_NUMBER}" =~ ^[0-9]+$ ]] || die "could not resolve PROJECT_NUMBER"

  SERVICE_ACCOUNT_EMAIL="${SERVICE_ACCOUNT_NAME}@${PROJECT_ID}.iam.gserviceaccount.com"
  TASKS_SERVICE_AGENT="service-${PROJECT_NUMBER}@gcp-sa-cloudtasks.iam.gserviceaccount.com"
  ROLE_NAME="projects/${PROJECT_ID}/roles/${ROLE_ID}"
  TASK_PREFIX="guard-${CLUSTER_NAME}"
}

setup_guard() {
  resolve_config

  if is_dry_run; then
    echo "Would configure the Cloud Tasks cost guard:"
    run "${GCLOUD}" services enable cloudtasks.googleapis.com container.googleapis.com iam.googleapis.com --project="${PROJECT_ID}" --quiet
    run "${GCLOUD}" beta services identity create --service=cloudtasks.googleapis.com --project="${PROJECT_ID}"
    run "${GCLOUD}" iam service-accounts create "${SERVICE_ACCOUNT_NAME}" --project="${PROJECT_ID}" --display-name="Ember GKE cost guard"
    run "${GCLOUD}" iam roles create "${ROLE_ID}" --project="${PROJECT_ID}" --title="Ember GKE cost guard" --stage=GA --permissions=container.clusters.delete,container.clusters.update
    run "${GCLOUD}" projects add-iam-policy-binding "${PROJECT_ID}" --member="serviceAccount:${SERVICE_ACCOUNT_EMAIL}" --role="${ROLE_NAME}" --condition=None --quiet
    run "${GCLOUD}" iam service-accounts add-iam-policy-binding "${SERVICE_ACCOUNT_EMAIL}" --project="${PROJECT_ID}" --member="serviceAccount:${TASKS_SERVICE_AGENT}" --role=roles/iam.serviceAccountUser --quiet
    run "${GCLOUD}" tasks queues create "${QUEUE_NAME}" --project="${PROJECT_ID}" --location="${TASKS_LOCATION}" --max-attempts=3 --max-retry-duration=3600s --min-backoff=60s --max-backoff=600s --max-concurrent-dispatches=1 --max-dispatches-per-second=1 --log-sampling-ratio=1
    return
  fi

  "${GCLOUD}" services enable cloudtasks.googleapis.com container.googleapis.com iam.googleapis.com --project="${PROJECT_ID}" --quiet
  "${GCLOUD}" beta services identity create --service=cloudtasks.googleapis.com --project="${PROJECT_ID}" >/dev/null

  if ! "${GCLOUD}" iam service-accounts describe "${SERVICE_ACCOUNT_EMAIL}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    "${GCLOUD}" iam service-accounts create "${SERVICE_ACCOUNT_NAME}" \
      --project="${PROJECT_ID}" \
      --display-name="Ember GKE cost guard"
  fi

  if "${GCLOUD}" iam roles describe "${ROLE_ID}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    "${GCLOUD}" iam roles update "${ROLE_ID}" \
      --project="${PROJECT_ID}" \
      --title="Ember GKE cost guard" \
      --description="Delete Ember GPU node pools and validation clusters after their TTL." \
      --stage=GA \
      --permissions=container.clusters.delete,container.clusters.update \
      --quiet >/dev/null
  else
    "${GCLOUD}" iam roles create "${ROLE_ID}" \
      --project="${PROJECT_ID}" \
      --title="Ember GKE cost guard" \
      --description="Delete Ember GPU node pools and validation clusters after their TTL." \
      --stage=GA \
      --permissions=container.clusters.delete,container.clusters.update \
      --quiet >/dev/null
  fi

  "${GCLOUD}" projects add-iam-policy-binding "${PROJECT_ID}" \
    --member="serviceAccount:${SERVICE_ACCOUNT_EMAIL}" \
    --role="${ROLE_NAME}" \
    --condition=None \
    --quiet >/dev/null
  "${GCLOUD}" iam service-accounts add-iam-policy-binding "${SERVICE_ACCOUNT_EMAIL}" \
    --project="${PROJECT_ID}" \
    --member="serviceAccount:${TASKS_SERVICE_AGENT}" \
    --role=roles/iam.serviceAccountUser \
    --quiet >/dev/null

  if "${GCLOUD}" tasks queues describe "${QUEUE_NAME}" --project="${PROJECT_ID}" --location="${TASKS_LOCATION}" >/dev/null 2>&1; then
    "${GCLOUD}" tasks queues update "${QUEUE_NAME}" \
      --project="${PROJECT_ID}" \
      --location="${TASKS_LOCATION}" \
      --max-attempts=3 \
      --max-retry-duration=3600s \
      --min-backoff=60s \
      --max-backoff=600s \
      --max-concurrent-dispatches=1 \
      --max-dispatches-per-second=1 \
      --log-sampling-ratio=1 >/dev/null
  else
    "${GCLOUD}" tasks queues create "${QUEUE_NAME}" \
      --project="${PROJECT_ID}" \
      --location="${TASKS_LOCATION}" \
      --max-attempts=3 \
      --max-retry-duration=3600s \
      --min-backoff=60s \
      --max-backoff=600s \
      --max-concurrent-dispatches=1 \
      --max-dispatches-per-second=1 \
      --log-sampling-ratio=1 >/dev/null
  fi

  echo "Cost guard ready in ${PROJECT_ID}: queue=${QUEUE_NAME}, serviceAccount=${SERVICE_ACCOUNT_EMAIL}"
}

future_time() {
  local hours=$1
  if date -u -d "+${hours} hours" '+%Y-%m-%dT%H:%M:%SZ' >/dev/null 2>&1; then
    date -u -d "+${hours} hours" '+%Y-%m-%dT%H:%M:%SZ'
    return
  fi
  if date -u -v+"${hours}"H '+%Y-%m-%dT%H:%M:%SZ' >/dev/null 2>&1; then
    date -u -v+"${hours}"H '+%Y-%m-%dT%H:%M:%SZ'
    return
  fi
  die "date does not support future-time calculation"
}

queue_exists() {
  "${GCLOUD}" tasks queues describe "${QUEUE_NAME}" \
    --project="${PROJECT_ID}" \
    --location="${TASKS_LOCATION}" >/dev/null 2>&1
}

delete_tasks() {
  local suffix=${1:-}
  local full_name task_id

  if is_dry_run; then
    echo "Would remove existing ${TASK_PREFIX}-*${suffix} tasks."
    return
  fi
  queue_exists || return

  while IFS= read -r full_name; do
    [[ -n "${full_name}" ]] || continue
    task_id=${full_name##*/}
    [[ "${task_id}" == "${TASK_PREFIX}-"* ]] || continue
    [[ -z "${suffix}" || "${task_id}" == *"${suffix}" ]] || continue
    "${GCLOUD}" tasks delete "${task_id}" \
      --project="${PROJECT_ID}" \
      --queue="${QUEUE_NAME}" \
      --location="${TASKS_LOCATION}" \
      --quiet
  done < <("${GCLOUD}" tasks list \
    --project="${PROJECT_ID}" \
    --queue="${QUEUE_NAME}" \
    --location="${TASKS_LOCATION}" \
    --format='value(name)')
}

delete_tasks_except() {
  local keep_one=$1
  local keep_two=$2
  local full_name task_id

  if is_dry_run; then
    echo "Would remove prior ${TASK_PREFIX}-* tasks after both replacement tasks exist."
    return
  fi

  while IFS= read -r full_name; do
    [[ -n "${full_name}" ]] || continue
    task_id=${full_name##*/}
    [[ "${task_id}" == "${TASK_PREFIX}-"* ]] || continue
    [[ "${task_id}" != "${keep_one}" && "${task_id}" != "${keep_two}" ]] || continue
    "${GCLOUD}" tasks delete "${task_id}" \
      --project="${PROJECT_ID}" \
      --queue="${QUEUE_NAME}" \
      --location="${TASKS_LOCATION}" \
      --quiet
  done < <("${GCLOUD}" tasks list \
    --project="${PROJECT_ID}" \
    --queue="${QUEUE_NAME}" \
    --location="${TASKS_LOCATION}" \
    --format='value(name)')
}

arm_guard() {
  resolve_config
  setup_guard

  if ! is_dry_run; then
    "${GCLOUD}" container clusters describe "${CLUSTER_NAME}" \
      --project="${PROJECT_ID}" \
      --location="${CLUSTER_LOCATION}" >/dev/null
    "${GCLOUD}" container node-pools describe "${GPU_NODE_POOL}" \
      --project="${PROJECT_ID}" \
      --cluster="${CLUSTER_NAME}" \
      --location="${CLUSTER_LOCATION}" >/dev/null
  fi

  local stamp gpu_task cluster_task gpu_delete_at cluster_delete_at
  local node_pool_url cluster_url
  stamp="$(date -u '+%Y%m%d%H%M%S')-$$"
  gpu_task="${TASK_PREFIX}-${stamp}-gpu"
  cluster_task="${TASK_PREFIX}-${stamp}-cluster"
  gpu_delete_at=$(future_time "${GPU_TTL_HOURS}")
  cluster_delete_at=$(future_time "${CLUSTER_TTL_HOURS}")
  node_pool_url="https://container.googleapis.com/v1/projects/${PROJECT_ID}/locations/${CLUSTER_LOCATION}/clusters/${CLUSTER_NAME}/nodePools/${GPU_NODE_POOL}"
  cluster_url="https://container.googleapis.com/v1/projects/${PROJECT_ID}/locations/${CLUSTER_LOCATION}/clusters/${CLUSTER_NAME}"

  run "${GCLOUD}" tasks create-http-task "${gpu_task}" \
    --project="${PROJECT_ID}" \
    --queue="${QUEUE_NAME}" \
    --location="${TASKS_LOCATION}" \
    --url="${node_pool_url}" \
    --method=DELETE \
    --schedule-time="${gpu_delete_at}" \
    --oauth-service-account-email="${SERVICE_ACCOUNT_EMAIL}" \
    --oauth-token-scope=https://www.googleapis.com/auth/cloud-platform
  run "${GCLOUD}" tasks create-http-task "${cluster_task}" \
    --project="${PROJECT_ID}" \
    --queue="${QUEUE_NAME}" \
    --location="${TASKS_LOCATION}" \
    --url="${cluster_url}" \
    --method=DELETE \
    --schedule-time="${cluster_delete_at}" \
    --oauth-service-account-email="${SERVICE_ACCOUNT_EMAIL}" \
    --oauth-token-scope=https://www.googleapis.com/auth/cloud-platform

  delete_tasks_except "${gpu_task}" "${cluster_task}"
  echo "GPU node pool deletion: ${gpu_delete_at}"
  echo "Cluster deletion:       ${cluster_delete_at}"
}

status_guard() {
  resolve_config
  if ! queue_exists; then
    echo "No cost-guard queue exists in ${PROJECT_ID}/${TASKS_LOCATION}."
    return
  fi

  local full_name scheduled task_id found=false
  printf 'TASK\tSCHEDULED_UTC\n'
  while IFS=$'\t' read -r full_name scheduled; do
    [[ -n "${full_name}" ]] || continue
    task_id=${full_name##*/}
    [[ "${task_id}" == "${TASK_PREFIX}-"* ]] || continue
    printf '%s\t%s\n' "${task_id}" "${scheduled}"
    found=true
  done < <("${GCLOUD}" tasks list \
    --project="${PROJECT_ID}" \
    --queue="${QUEUE_NAME}" \
    --location="${TASKS_LOCATION}" \
    --format='value(name,scheduleTime)')
  if [[ "${found}" == "false" ]]; then
    echo "(none)"
  fi
}

disarm_guard() {
  resolve_config
  delete_tasks
  echo "Deletion tasks removed for ${CLUSTER_NAME}."
}

destroy_gpu() {
  resolve_config
  local expected="${PROJECT_ID}/${CLUSTER_LOCATION}/${CLUSTER_NAME}/${GPU_NODE_POOL}"
  [[ "${CONFIRM_DESTROY:-}" == "${expected}" ]] || die "set CONFIRM_DESTROY=${expected}"

  if is_dry_run || "${GCLOUD}" container node-pools describe "${GPU_NODE_POOL}" --project="${PROJECT_ID}" --cluster="${CLUSTER_NAME}" --location="${CLUSTER_LOCATION}" >/dev/null 2>&1; then
    run "${GCLOUD}" container node-pools delete "${GPU_NODE_POOL}" \
      --project="${PROJECT_ID}" \
      --cluster="${CLUSTER_NAME}" \
      --location="${CLUSTER_LOCATION}" \
      --quiet
  else
    echo "GPU node pool ${GPU_NODE_POOL} does not exist."
  fi
  delete_tasks -gpu
}

destroy_all() {
  resolve_config
  local expected="${PROJECT_ID}/${CLUSTER_LOCATION}/${CLUSTER_NAME}"
  [[ "${CONFIRM_DESTROY:-}" == "${expected}" ]] || die "set CONFIRM_DESTROY=${expected}"

  if is_dry_run || "${GCLOUD}" container clusters describe "${CLUSTER_NAME}" --project="${PROJECT_ID}" --location="${CLUSTER_LOCATION}" >/dev/null 2>&1; then
    run "${GCLOUD}" container clusters delete "${CLUSTER_NAME}" \
      --project="${PROJECT_ID}" \
      --location="${CLUSTER_LOCATION}" \
      --quiet
  else
    echo "Cluster ${CLUSTER_NAME} does not exist."
  fi
  delete_tasks
}

case "${1:-}" in
  setup)
    setup_guard
    ;;
  arm)
    arm_guard
    ;;
  status)
    status_guard
    ;;
  disarm)
    disarm_guard
    ;;
  destroy-gpu)
    destroy_gpu
    ;;
  destroy-all)
    destroy_all
    ;;
  *)
    usage
    exit 1
    ;;
esac
