#!/usr/bin/env bash
set -euo pipefail

GCLOUD=${GCLOUD:-gcloud}
PROJECT_ID=${PROJECT_ID:-}
REGION=${REGION:-us-central1}
REPOSITORY=${REPOSITORY:-ember}
IMAGE_TAG=${IMAGE_TAG:-}
ALLOW_DIRTY=${ALLOW_DIRTY:-false}

usage() {
  cat <<'EOF'
Usage: scripts/gke-build-images.sh

Build the reviewed Ember control-plane images as linux/amd64 images in Artifact Registry.
The worktree must be clean unless ALLOW_DIRTY=true is set explicitly.
EOF
}

die() {
  echo "gke image build: $*" >&2
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

require_command "${GCLOUD}"
require_command git

if [[ -z "${PROJECT_ID}" ]]; then
  PROJECT_ID=$("${GCLOUD}" config get-value project 2>/dev/null || true)
fi
[[ "${PROJECT_ID}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] || die "invalid PROJECT_ID"
[[ "${REGION}" =~ ^[a-z]+-[a-z]+[0-9]+$ ]] || die "invalid REGION"
[[ "${REPOSITORY}" =~ ^[a-z][a-z0-9-]{0,62}$ ]] || die "invalid REPOSITORY"

if [[ -z "${IMAGE_TAG}" ]]; then
  IMAGE_TAG=$(git rev-parse --short=12 HEAD)
fi
[[ "${IMAGE_TAG}" =~ ^[A-Za-z0-9._-]{1,128}$ ]] || die "invalid IMAGE_TAG"

if [[ "${ALLOW_DIRTY}" != "true" && "${ALLOW_DIRTY}" != "1" ]] && [[ -n "$(git status --porcelain)" ]]; then
  die "the worktree is dirty; commit first so IMAGE_TAG identifies the exact source, or set ALLOW_DIRTY=true"
fi

"${GCLOUD}" services enable cloudbuild.googleapis.com artifactregistry.googleapis.com \
  --project="${PROJECT_ID}" \
  --quiet
if ! "${GCLOUD}" artifacts repositories describe "${REPOSITORY}" \
  --project="${PROJECT_ID}" \
  --location="${REGION}" >/dev/null 2>&1; then
  "${GCLOUD}" artifacts repositories create "${REPOSITORY}" \
    --project="${PROJECT_ID}" \
    --location="${REGION}" \
    --repository-format=docker \
    --description="Ember reviewed runtime images"
fi

registry="${REGION}-docker.pkg.dev/${PROJECT_ID}/${REPOSITORY}"
"${GCLOUD}" builds submit . \
  --project="${PROJECT_ID}" \
  --region="${REGION}" \
  --config=deploy/gke/cloudbuild.yaml \
  --substitutions="_REGISTRY=${registry},_IMAGE_TAG=${IMAGE_TAG}"

echo "Built amd64 images in ${registry} with tag ${IMAGE_TAG}."
