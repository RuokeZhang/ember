# Ember

Ember is a Kubernetes control plane for GPU inference endpoints. It converts an allowlisted model and serving profile into an observable, GPU-scheduled, autoscaled, and reclaimable endpoint.

The implementation follows [`Ember_Kubernetes_Design_Doc.md`](./Ember_Kubernetes_Design_Doc.md).

## Current milestone

The local Kind milestone establishes the control plane and the Phase 4 product surface:

- `serving.ember.dev/v1alpha1` `InferenceEndpoint` API.
- `ModelCache` reconciliation with verified synthetic safetensors materialization.
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
  docker run --rm -i -v "$PWD":/workspace -w /workspace golang:1.24 \
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
