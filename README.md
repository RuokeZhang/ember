# Ember

Ember is a Kubernetes control plane for GPU inference endpoints. It converts an allowlisted model and serving profile into an observable, GPU-scheduled, autoscaled, and reclaimable endpoint.

The implementation follows [`Ember_Kubernetes_Design_Doc.md`](./Ember_Kubernetes_Design_Doc.md).

## Current milestone

The Kind control-plane and product milestones are complete. The real-runtime path is implemented and awaiting measured GKE validation:

- `serving.ember.dev/v1alpha1` `InferenceEndpoint` API.
- `ModelCache` reconciliation with verified synthetic or immutable real-model materialization.
- A compile-time allowlist for the nine runtime files in `Qwen/Qwen2.5-7B-Instruct-AWQ` revision `b25037543e9394b818fdfca67ab2a00ecc7dd641`.
- Streaming per-file download, size and SHA-256 verification, bounded safetensors header validation, fsync, and atomic cache publication.
- A digest-pinned official `vllm/vllm-openai:v0.8.5` runtime that loads only the read-only local cache with remote access and usage telemetry disabled.
- Cache state encoded as node labels and required hostname affinity for warm placement.
- Read-only serving mounts with a non-root cache verification init container.
- An idempotent Go operator and finalizer-driven cleanup.
- GPU-aware workload resources with quota, networking, and locked-down service accounts.
- An in-repository fake GPU device plugin for Kind.
- A mock OpenAI-compatible inference engine for lifecycle tests without GPU cost.
- An authenticated Gateway with owner isolation, status streaming, bounded logs, and inference proxying.
- A Postgres-backed Control API with opaque demo sessions, server-side catalog validation, deterministic idempotent endpoint creation, retained presentation metadata, and append-only audit history.
- Prometheus discovery of per-endpoint engine metrics.
- KEDA queue-depth autoscaling for replicas `1..maxReplicas`.
- Operator-owned idle transition to zero and Gateway-triggered activation back to one.
- A React/TypeScript product UI served by the Control API from the same origin.
- Fleet and endpoint dashboards backed by authoritative CR status, Prometheus time series, Kubernetes API inspection, append-only audit records, and bounded engine logs.
- OpenAI-compatible chat with SSE streaming and a browser-driven concurrent Load Lab.
- A GKE overlay plus amd64 Cloud Build, one-L4 Spot cluster automation, and a focused real-runtime smoke test.

Performance numbers in the design document are targets until they are measured on GKE with a real NVIDIA L4 and vLLM. Simulated results are never presented as GPU performance.

## Product UI

After deploying to Kind, expose the same-origin web application:

```sh
kubectl -n ember-system port-forward service/ember-control-api 18080:8080
```

Open [http://127.0.0.1:18080](http://127.0.0.1:18080). The interface can create and delete endpoints, follow lifecycle conditions, chat through the OpenAI-compatible route, generate concurrent load, graph queue/replica history, inspect generated Kubernetes resources, and read logs plus audit evidence.

The dashboard does not invent data:

| Surface | Authoritative source |
|---|---|
| Lifecycle, replicas, placement, cache state | `InferenceEndpoint.status` |
| Queue depth, running requests, request count | Prometheus |
| Namespace, Deployment, Service, quota, KEDA/HPA, Pods, policies, events | Kubernetes API through the owner-scoped Gateway |
| Product actions | Append-only Postgres audit history |
| Engine output | Bounded and redacted Pod logs |

The GPU allocation meter and browser-observed TTFT samples are explanatory demo evidence, not billing data or real-GPU benchmark claims.

## Safety

Ember accepts only reviewed catalog entries. It does not accept arbitrary model URLs, images, Python code, engine flags, or Kubernetes manifests.

The model-cache design requires a narrow `hostPath` exception. Kubernetes Baseline and Restricted Pod Security both reject `hostPath`, so the implementation uses privileged PSA only for generated workload namespaces and restores a narrow serving-pod shape through admission policy. Prefetch runs in `ember-system`; a minimal init container retains only `CHOWN` to prepare the kubelet-created cache root, while the downloader itself remains non-root and capability-free. This is a documented MVP compromise, not a claim of hostile multi-tenant isolation.

## Development

All dependencies and test components are built from repository source or pinned upstream modules. The Kind path does not download or execute a third-party GPU operator or installation script.

```sh
make verify
```

Docker is used for deterministic Go tooling when a host Go installation is unavailable.

### GCP cost guard

Budget alerts are advisory and can arrive after spend occurs. Before creating a real GPU node pool, install the cloud-side cost guard:

```sh
make gcp-cost-guard-setup GCP_PROJECT=your-project-id
```

The setup creates a dedicated keyless service account, a project custom role containing only `container.clusters.update` and `container.clusters.delete`, and a Cloud Tasks queue. After the GKE cluster and L4 node pool exist, arm one-shot deletion tasks:

```sh
make gcp-cost-guard-arm \
  GCP_PROJECT=your-project-id \
  GKE_CLUSTER=ember-gpu \
  GKE_LOCATION=us-central1-a \
  GKE_GPU_NODE_POOL=l4-spot
```

By default, the GPU node pool is deleted after three hours and the whole validation cluster after six hours. Cloud Tasks removes each task after successful execution, so no recurring deletion job remains. Inspect or intentionally remove the timers with:

```sh
make gcp-cost-guard-status GCP_PROJECT=your-project-id
make gcp-cost-guard-disarm GCP_PROJECT=your-project-id
```

For immediate teardown, the destructive target requires the full resource identity as an explicit confirmation:

```sh
CONFIRM_DESTROY=your-project-id/us-central1-a/ember-gpu \
  make gcp-cost-guard-destroy GCP_PROJECT=your-project-id
```

The recommended project budget is USD 50 per month with alerts at 20%, 50%, 80%, and 100%, plus a forecasted 100% alert. Configure it to exclude credits from spend calculations so the USD 300 trial credit does not hide resource consumption. A budget is not a spending cap; the one-shot deletion tasks are the hard runtime protection.

### GKE L4 lifecycle

The GKE scripts require `gcloud`, `gke-gcloud-auth-plugin`, `kubectl`, Docker, and `jq`. Install the Google-authored plugin with `gcloud components install gke-gcloud-auth-plugin` before creating the cluster.

First, after committing the exact source to build, create four Linux amd64 repository images in Artifact Registry. The build script refuses a dirty worktree by default so the image tag identifies the source commit:

```sh
make gke-build-images GCP_PROJECT=your-project-id
```

The GKE path creates one zonal Standard cluster with an `e2-standard-4` CPU node and one autoscaled `g2-standard-8` Spot node containing a single NVIDIA L4. This incurs real charges. The cluster script configures the cost guard and arms the three-hour GPU-pool and six-hour cluster deletion tasks before it requests the GPU:

```sh
make gke-cluster-create GCP_PROJECT=your-project-id
```

Deploy the real-mode overlay. The script resolves every repository image tag to its Artifact Registry digest before applying it, generates the cluster secrets locally, and installs the digest-verified KEDA release:

```sh
make gke-deploy GCP_PROJECT=your-project-id
```

Run the focused hardware smoke test:

```sh
make gke-real-smoke GCP_PROJECT=your-project-id
```

The smoke test refreshes the cleanup timers, ensures one L4 node is running, waits up to 30 minutes for the 5.58 GB verified cache and vLLM startup, sends one OpenAI-compatible chat request, labels its wall time as a non-benchmark sample, then deletes the endpoint and resizes the GPU pool to zero. Set `KEEP_RESOURCES=true` only when you intentionally need to inspect the live endpoint; the previously armed TTL tasks still apply.

The real model artifact is the deterministic manifest digest `sha256:41d12f80b6d62f01e9134f410ab177d907ccb025e41bbb651bd83e8e8304f010`. The official amd64 vLLM image is pinned to `sha256:6cf9808ca8810fc6c3fd0451c2e7784fb224590d81f7db338e7eaf3c02a33d33`. No `--trust-remote-code` path is enabled.

### Kind lifecycle

Build the six repository-owned images, create the four-node simulation cluster, and deploy. `make deploy` installs the pinned KEDA 2.17.2 release after verifying its manifest digest:

```sh
make images
make kind-create KIND=/path/to/kind
make kind-load KIND=/path/to/kind
make deploy
make sample
kubectl -n ember-system wait --for=condition=Ready \
  inferenceendpoint/ep-7f92c8 --timeout=180s
```

Run the repeatable cache materialization, warm-cache reuse, authenticated inference, self-heal, Prometheus/KEDA scale-up, idle scale-to-zero, Gateway reactivation, and reclamation sequence with:

```sh
make kind-smoke
```

Validate the browser-facing boundary separately. This checks the built SPA and assets, opaque cookie session, idempotent creation, owner-scoped Kubernetes inspection, Prometheus metrics, ordinary and SSE inference, Load Lab scale-up, logs, append-only audit behavior, Postgres/API restart recovery, deletion, and retained metadata:

```sh
make control-api-smoke
```

The Kind database uses a retained 2 GiB node-local volume and a randomly generated Kubernetes Secret. It is a local persistence test, not the production Postgres topology.

The Kind topology contains one CPU worker and two tainted, L4-labeled fake GPU workers. The in-repository device plugin advertises two `nvidia.com/gpu` devices on each GPU worker. Simulation mode preserves GPU requests, selectors, and taints while reducing CPU and memory requests so the mock workload fits locally.

Inspect the endpoint and generated workload:

```sh
kubectl -n ember-system get inferenceendpoint ep-7f92c8
kubectl get pods -A -l ember.dev/endpoint-uid -o wide
```

Forward the Gateway and issue a short-lived owner token without writing the private key to disk:

```sh
kubectl -n ember-system port-forward service/ember-gateway 8080:8080

token=$(
  kubectl -n ember-system get secret ember-jwt-keys \
    -o jsonpath='{.data.private\.key}' |
  docker run --rm -i -v "$PWD":/workspace -w /workspace \
    golang:1.24@sha256:d2d2bc1c84f7e60d7d2438a3836ae7d0c847f4888464e7ec9ba3a1339a1ee804 \
    go run ./cmd/auth-tool token \
      --private-key-base64-stdin \
      --subject usr_31d2 \
      --ttl 60s
)

curl -sS http://127.0.0.1:8080/v1/endpoints/ep-7f92c8/v1/chat/completions \
  -H "Authorization: Bearer $token" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen2.5-7b-instruct-awq","messages":[{"role":"user","content":"What is Ember?"}]}'
```

Deleting the CR scales the Deployment to zero, waits for Pods to disappear, deletes the generated namespace, and only then removes the finalizer:

```sh
kubectl -n ember-system delete inferenceendpoint ep-7f92c8
```

For the product API, forward the Control API and let it issue the demo session. The browser-facing request never carries a gateway JWT:

```sh
kubectl -n ember-system port-forward service/ember-control-api 8082:8080
curl -sS -c /tmp/ember.cookies -X POST http://127.0.0.1:8082/api/v1/session
curl -sS -b /tmp/ember.cookies http://127.0.0.1:8082/api/v1/catalog/models
```

Kind's default CNI does not enforce `NetworkPolicy`; these manifests demonstrate policy intent, not traffic-level isolation. Enforcement must be tested separately on a policy-capable CNI.
