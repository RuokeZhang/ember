#!/usr/bin/env bash
set -euo pipefail

KUBECTL=${KUBECTL:-kubectl}
KEDA_VERSION=${KEDA_VERSION:-2.17.2}
KEDA_SHA256=${KEDA_SHA256:-1912c29059e41e80b6abe8b29a566e07337f85fc4bf4fd5698c637fe63ecbf35}
KEDA_URL="https://github.com/kedacore/keda/releases/download/v${KEDA_VERSION}/keda-${KEDA_VERSION}.yaml"

wait_for_keda() {
  "${KUBECTL}" -n keda rollout status deployment/keda-operator --timeout=180s
  "${KUBECTL}" -n keda rollout status deployment/keda-metrics-apiserver --timeout=180s
  "${KUBECTL}" -n keda rollout status deployment/keda-admission --timeout=180s
}

if "${KUBECTL}" get crd scaledobjects.keda.sh >/dev/null 2>&1 &&
  "${KUBECTL}" -n keda get deployment keda-operator keda-metrics-apiserver keda-admission >/dev/null 2>&1; then
  wait_for_keda
  exit 0
fi

manifest=$(mktemp)
cleanup() {
  rm -f "${manifest}"
}
trap cleanup EXIT

curl -fsSL "${KEDA_URL}" -o "${manifest}"
if command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "${manifest}" | awk '{print $1}')
else
  actual=$(sha256sum "${manifest}" | awk '{print $1}')
fi
if [[ "${actual}" != "${KEDA_SHA256}" ]]; then
  echo "KEDA manifest digest mismatch: got ${actual}, expected ${KEDA_SHA256}" >&2
  exit 1
fi

"${KUBECTL}" apply --server-side -f "${manifest}"
wait_for_keda
