# Ember: A Kubernetes Control Plane for GPU Inference Endpoints

**Status:** Draft for technical review
**Author:** Ruoke Zhang
**Date:** August 15, 2026
**Target:** Portfolio project. Kubernetes control plane on GKE; product surface hosted on Replit.

**Implementation status:** The repository implements the Kind control plane and Phase 4 product surface plus the real-runtime transition: immutable Qwen file manifests, streaming verified prefetch, a digest-pinned official vLLM image, a real-mode GKE overlay, amd64 Cloud Build, one-L4 Spot cluster automation, and automatic cleanup. Kind still uses the in-repository fake GPU device plugin, synthetic safetensors, and mock OpenAI-compatible engine. Real NVIDIA L4 measurements, Replit hosting, and the optional Phase 5 items remain future validation work; no simulated timing is presented as real GPU performance.

---

## 1. Executive Summary

Ember is a small Kubernetes control plane that turns a declared model-serving intent into a running, scheduled, autoscaled, and reclaimable GPU inference endpoint.

A user selects an allowlisted open-weight model and a serving profile. Ember creates an `InferenceEndpoint` custom resource. A Go operator reconciles that desired state into GPU-scheduled workloads, model-weight prefetch, node-cache-aware placement, a routable OpenAI-compatible endpoint, queue-depth autoscaling, idle scale-to-zero, and complete resource reclamation.

The central engineering artifact is a Go operator implementing an idempotent reconciliation loop over a versioned `InferenceEndpoint` CRD. Two behaviors are the technical core and everything else exists to support them:

1. **Cache-aware placement.** Model weights are tens of gigabytes. Where a serving Pod lands determines whether it starts in ninety seconds or six minutes. The operator tracks per-node weight cache state as node labels and biases scheduling toward warm nodes, with a bounded fallback to cold nodes when warm capacity is unavailable.
2. **Scale-to-zero with honest cold start.** Idle endpoints release their GPUs. The next request re-materializes the workload. The system reports the resulting cold-start latency rather than hiding it.

The MVP serves allowlisted open-weight models with safetensors artifacts on a single-region GKE cluster with L4 nodes. It does **not** claim production multi-tenant GPU isolation, does not accept arbitrary user-supplied model code, and does not implement disaggregated prefill/decode or KV-cache-aware request routing. Those are named as future work with reasons.

---

## 2. Motivation

Any team running LLM inference on Kubernetes repeatedly solves the same coupled problems:

1. Convert a request such as "serve this model at this size" into GPU-scheduled infrastructure.
2. Get the weights onto the right node fast enough that cold start is tolerable.
3. Schedule an indivisible multi-GPU workload without deadlocking against other tenants.
4. Scale on a signal that actually correlates with saturation, which is not CPU utilization.
5. Reclaim expensive accelerators the moment they stop earning their cost.
6. Report failures in terms an operator can act on: out of GPU capacity, weights unavailable, OOM during load, rollout stuck.

A conventional deployment tool hides these behind a managed provider. Ember exposes them as first-class controller behavior. It exercises Kubernetes API extension, declarative reconciliation, accelerator-aware scheduling, node-local cache management, custom-metric autoscaling, scale-from-zero, rollout safety, resource governance, and observability specific to inference workloads.

This is small enough to demonstrate end to end. A reviewer can create an endpoint, watch the weight prefetch Job populate a node cache, send a completion request, watch the endpoint scale up under load and back to zero when idle, and confirm that every GPU and every object is released.

### 2.1 Relationship to Existing Systems

KServe, llm-d, AIBrix, and Ray Serve solve overlapping problems in production. Ember does not compete with them and the documentation says so explicitly. Ember rebuilds a minimal version of the control plane those systems provide, in order to demonstrate understanding of the mechanisms rather than familiarity with a Helm chart. Section 24.1 states which parts a production deployment should delegate to those projects instead.

---

## 3. Goals and Non-Goals

### 3.1 Goals

- Represent serving intent with a versioned `InferenceEndpoint` CRD.
- Implement an idempotent Go controller reconciling actual state toward desired state.
- Schedule serving Pods against `nvidia.com/gpu`, respecting node taints, GPU model selectors, per-endpoint resource quota, and per-owner endpoint limits.
- Prefetch model weights to node-local cache and expose cache state as node labels.
- Bias Pod placement toward nodes with a warm cache, with bounded fallback and no scheduling deadlock.
- Serve an OpenAI-compatible HTTP surface through a stable in-cluster route.
- Autoscale replicas on queue depth rather than CPU.
- Scale to zero on idle and cold-start correctly on the next request.
- Roll out a new model revision without dropping in-flight requests.
- Reclaim GPUs, Pods, routes, and cache entries; prove reclamation is complete.
- Surface actionable status conditions and reason codes for every failure class.
- Report measured cold-start, warm-start, TTFT, and reclamation numbers with the workload and cluster fully described.
- Keep the public Replit service outside the Kubernetes trust boundary.

### 3.2 Proposed MVP Targets

Design targets, not measured claims. Final documentation must report observed numbers with the cluster, GPU type, model, quantization, prompt length, and sample count stated.

Reference workload: `Qwen2.5-7B-Instruct` (AWQ 4-bit, approximately 5.5 GB of safetensors) on a single NVIDIA L4 24 GB, vLLM backend, 128-token prompt, 128-token completion.

| Metric | MVP target | Scope |
|---|---:|---|
| Warm start, node cache hit, p95 | < 90 s | CR create to `Ready=True` |
| Cold start, cache miss from object storage, p95 | < 5 min | Includes weight download |
| Scale-from-zero to first token, warm cache | < 120 s | Request arrival to first streamed token |
| TTFT at concurrency 1, p95 | < 500 ms | Steady state, endpoint already Ready |
| Cache hit rate after warmup | > 90 % | Over the integration test sequence |
| Idle scale-down | within 60 s of `idleTimeoutSeconds` | GPU released, verified by node allocatable |
| Accepted request to terminal state | < 8 min | `Ready` or actionable failed condition |
| Leaked GPUs, Pods, PVs, routes | 0 after full test suite | Includes injected operator restart |
| Concurrent endpoints | 4 on a 2-node L4 cluster | Two must contend and one must correctly report `InsufficientGPU` |
| Control-plane availability | Best effort | Portfolio demo, not a production SLO |

### 3.3 Non-Goals

- Production-grade multi-tenant GPU isolation against hostile tenants.
- Accepting arbitrary user-supplied model code, custom Python handlers, or pickle-format checkpoints.
- Training, fine-tuning, or any gradient computation.
- Disaggregated prefill/decode serving.
- KV-cache-aware or prefix-aware request routing across replicas.
- Multi-region, multi-cluster, or cross-cloud scheduling.
- Fractional GPU sharing via MPS, MIG partitioning, or time-slicing.
- Speculative decoding, custom kernels, or any inference-engine-internal optimization. Ember treats the engine as a black box with a known interface.
- Billing, quotas per paying customer, or enterprise identity integration.

The MIG and fractional-sharing exclusion deserves a note: it is the single most attractive extension for an AI infrastructure portfolio, and it is excluded from the MVP only because it requires hardware the project budget does not cover. Section 23 lists it as the first post-MVP item.

---

## 4. User Experience

### 4.1 Primary Flow

1. The user opens the Ember web application deployed through Replit.
2. The user selects an allowlisted model and a serving profile (`small`, `standard`, or `tp2`), plus an idle timeout.
3. The UI displays the resolved engine image, GPU count, expected memory footprint, and whether any node currently holds a warm cache for this model.
4. The user selects **Create Endpoint**.
5. The page moves through `Accepted → Scheduling → LoadingWeights → Warming → Ready`, with the current condition, reason, and elapsed time visible at each step.
6. When ready, the page exposes a chat box and a `curl` snippet against the OpenAI-compatible route.
7. A load generator button drives concurrent requests; the user watches queue depth rise, a second replica appear, and TTFT change.
8. After the idle timeout the endpoint transitions to `ScaledToZero`. GPU allocatable on the node visibly returns.
9. The next request triggers a cold start; the UI shows the resulting latency without smoothing it.
10. The user deletes the endpoint and the UI shows `Terminating → Deleted` only after cluster reclamation is confirmed.

### 4.2 Failure Flow

The UI never displays a bare `Pod failed`. Every failure resolves to a stable reason with evidence and a suggested action.

```text
Condition: Ready=False
Reason: InsufficientGPU
Message: Endpoint requests 2 nvidia.com/gpu; cluster has 1 allocatable
         GPU across 2 schedulable nodes.
Evidence: Pod ember-ep-7f92c8-0 Unschedulable for 94s.
          Node gke-l4-a: 1/1 GPUs allocated to endpoint ember-ep-31d2.
Suggested action: Delete an existing endpoint, or select the `small`
                  profile which requests 1 GPU.
```

```text
Condition: Ready=False
Reason: OOMDuringLoad
Message: Engine process exited with code 137 while loading weights.
Evidence: Model reports 15.2 GiB of parameters at bf16; the selected
          profile allocates a 24 GiB L4 with 0.90 gpu_memory_utilization.
Suggested action: Select an AWQ or GPTQ revision of this model, or the
                  `tp2` profile.
```

This section is deliberately over-specified. Turning Kubernetes events and engine stderr into these two paragraphs is a meaningful fraction of the project's value, and reviewers of the running demo will see it before they see any code.

---

## 5. System Architecture

```text
Browser
  |
  | HTTPS / SSE
  v
Replit-hosted Web + Control API          [outside cluster, no k8s credentials]
  |  - user-facing metadata, Postgres
  |  - request validation against allowlists
  |  - short-lived signed JWT calls to gateway
  v
In-cluster Endpoint Gateway              [security boundary]
  |  - authenticates caller, maps to ownerID
  |  - allowlisted operations only
  |  - enforces ownership on every read and mutation
  |  - proxies inference traffic and bounded logs
  v
Kubernetes API Server                    [GKE managed control plane, always on]
  ^
  |
Ember Operator (Go / controller-runtime)
  |  - reconciles InferenceEndpoint CRs
  |  - drives weight prefetch and node cache labels
  |  - computes cache-aware placement
  |  - manages scale-to-zero and rollout
  |  - publishes status conditions and metrics
  |
  +---> Per-endpoint workload namespace
  |       |- ResourceQuota (nvidia.com/gpu, memory, pods)
  |       |- privileged PSA label plus narrow admission policy
  |       |- default-deny NetworkPolicy
  |       |- serving Deployment (or LeaderWorkerSet for tp > 1)
  |       |- Service (ClusterIP) + HTTPRoute
  |       `- ServiceMonitor
  |
  +---> ember-system namespace
          |- ModelCache CRs (one per model x node-pool)
          |- weight prefetch Jobs
          `- cache-reaper CronJob

GPU node pool (spot, scale-from-zero via cluster-autoscaler)
  |- nvidia.com/gpu=true taint, ember.dev/gpu=l4 label
  |- node-local NVMe cache at /var/lib/ember/models
  `- node labels: cache.ember.dev/<model-hash>=ready|loading
```

### 5.1 Replit-hosted Web and Control API

Responsibilities:

- Serve the React/TypeScript interface.
- Authenticate the demo user and issue a session.
- Validate model ID, profile, idle timeout, and replica bounds against server-side allowlists.
- Persist presentation metadata, ownership, and an append-only audit trail in Postgres.
- Call the in-cluster gateway with a short-lived signed bearer token.
- Proxy bounded status streams, metrics, and inference requests without exposing Kubernetes credentials to the browser.

The web service is not the source of truth for runtime state. `InferenceEndpoint.status` is authoritative. Postgres stores presentation metadata and audit history that must outlive the CR.

The anonymous demo session is an opaque, cryptographically random token stored in an `HttpOnly`, `SameSite=Strict`, `Secure` cookie. Postgres stores only its SHA-256 hash, owner ID, expiry, and revocation timestamp; the default lifetime is 24 hours. The Kind HTTP overlay disables only the `Secure` attribute for local port-forward testing. Browsers never receive the short-lived gateway JWT or its signing key.

Endpoint creation requires an `Idempotency-Key`. The control API atomically reserves a presentation row and deterministic `ep-...` ID before calling the gateway. Repeating the same key and normalized request returns the same endpoint; reusing the key with different input returns `409 Conflict`. The gateway accepts that preallocated ID and treats an existing endpoint with the same owner and spec as a successful replay. This closes the failure window where the CR is created but the public API loses the response.

### 5.2 Endpoint Gateway

A small Go service in the cluster. It exists because the public Replit application must not hold a kubeconfig, and because RBAC alone cannot express the authorization rule the product needs.

That last point is the actual justification and the design should state it. Kubernetes RBAC authorizes verbs on resource types within a namespace. It cannot express "this user may read only the `InferenceEndpoint` objects whose `spec.ownerID` matches their identity." Without a namespace per user, per-object ownership filtering must live in an application-layer proxy. The gateway is that proxy.

Operations exposed:

- Create a validated `InferenceEndpoint`.
- Get one endpoint and its status.
- Delete one endpoint owned by the caller.
- Watch status for a bounded duration.
- Read bounded, redacted engine logs.
- Proxy inference requests to the endpoint's Service, subject to ownership and rate limits.

Authentication uses a short-lived signed JWT issued by the control API and verified by the gateway against a configured public key, with `exp`, `aud`, `sub`, and `jti` claims. This is application-issued JWT authentication, not OIDC; adding a real identity provider is outside the anonymous portfolio-demo scope. **Nonce-based replay protection is deliberately omitted from the MVP.** The original design included it; it adds meaningful custom authentication code, contributes no Kubernetes signal, and a sixty-second token lifetime over TLS is proportionate to a portfolio demo. This is recorded as an explicit, documented gap rather than an oversight.

The gateway ServiceAccount has RBAC for `InferenceEndpoint` objects in `ember-system` and `pods/log` in Ember-managed workload namespaces, and nothing else. It cannot create Pods, read Secrets, create namespaces, or touch namespaces outside the managed label selector.

### 5.3 Ember Operator

Deployed in `ember-system`. `InferenceEndpoint` objects also live in `ember-system`; each endpoint receives a generated workload namespace recorded in status. `ModelCache` remains cluster-scoped. This avoids the impossible bootstrap cycle where a namespaced CR would need to exist before the operator could create its namespace, and it keeps namespace mutation out of the public gateway. Two controllers:

- **EndpointController** owns the serving workload, route, autoscaling, rollout, idle handling, and reclamation.
- **CacheController** owns weight prefetch Jobs, node cache labels, and cache eviction.

They communicate only through the API server. `EndpointController` reads node labels and `ModelCache.status` to compute placement; it never calls `CacheController` directly. This keeps both controllers independently testable and restart-safe.

Reconciliation contract:

```text
Reconcile(observed cluster state, InferenceEndpoint spec)
  -> make bounded progress toward desired state
  -> publish observable status
  -> return safely when invoked again
```

The loop must be idempotent. Child names derive from the immutable CR UID. All managed objects carry `ember.dev/endpoint-uid` and `ember.dev/owner` labels. A controller restart or duplicate event must not produce a second Deployment or a second prefetch Job.

### 5.4 Serving Runtime

The MVP uses a pinned vLLM OpenAI-server image. Ember supplies configuration; it never supplies model code.

Each serving Pod:

1. Requests `nvidia.com/gpu: <profile.gpuCount>` and tolerates the GPU node taint.
2. Mounts the node-local weight cache read-only at `/models`.
3. Runs an init container that verifies the expected weight files and digests exist in the cache, and fails fast with `CacheMiss` if not — the placement logic should have prevented this, and a failure here is a bug worth surfacing loudly.
4. Starts the engine with `--model /models/cache`, `--served-model-name <public-id>`, and profile-derived flags. No user-supplied engine arguments.
5. Uses vLLM's `/health` endpoint behind a bounded 15-minute startup probe, followed by readiness and liveness probes. The separate hardware smoke test sends a real chat completion before the validation run is accepted.
6. Runs as non-root with a read-only root filesystem, writable `/tmp` and `/dev/shm` only.

The `/dev/shm` exception matters: tensor-parallel serving uses shared memory for inter-rank communication and the Kubernetes default of 64 MB will hang a `tp>1` deployment in a way that looks like a scheduling problem. The design allocates a `Memory`-backed `emptyDir` at `/dev/shm` sized by profile.

---

## 6. `InferenceEndpoint` API Design

### 6.1 Example Resource

```yaml
apiVersion: serving.ember.dev/v1alpha1
kind: InferenceEndpoint
metadata:
  name: ep-7f92c8
  namespace: ember-system
spec:
  ownerID: usr_31d2
  model:
    id: qwen2.5-7b-instruct-awq        # allowlisted catalog key
    revision: b25037543e9394b818fdfca67ab2a00ecc7dd641  # immutable upstream commit
  profile: standard                     # small | standard | tp2
  scaling:
    minReplicas: 0
    maxReplicas: 3
    targetQueueDepth: 4
    idleTimeoutSeconds: 900
  placement:
    cachePreference: Preferred          # Preferred | Required
    maxColdStartFallbackSeconds: 120
status:
  observedGeneration: 3
  phase: Ready                          # derived; conditions are authoritative
  workloadNamespace: ember-ep-4d71a9
  endpointURL: https://ember.example.dev/v1/endpoints/ep-7f92c8
  replicas:
    desired: 1
    ready: 1
  placement:
    node: gke-ember-l4-spot-a3f1
    cacheState: Hit
  model:
    resolvedDigest: sha256:41d12f80b6d62f01e9134f410ab177d907ccb025e41bbb651bd83e8e8304f010
    sizeBytes: 5582381128
  lastActivityTime: "2026-08-15T20:47:03Z"
  conditions:
    - type: Ready
      status: "True"
      reason: EngineServing
      message: Synthetic completion succeeded in 312ms.
      observedGeneration: 3
      lastTransitionTime: "2026-08-15T20:31:12Z"
    - type: Progressing
      status: "False"
      reason: RolloutComplete
      observedGeneration: 3
      lastTransitionTime: "2026-08-15T20:31:12Z"
    - type: Degraded
      status: "False"
      reason: AsExpected
      observedGeneration: 3
      lastTransitionTime: "2026-08-15T20:31:12Z"
```

### 6.2 Spec Design Decisions

- `ownerID` is immutable and injected by the gateway. Never trusted from browser input.
- `InferenceEndpoint` resources live only in `ember-system`. The operator creates a UID-derived workload namespace and records it in `status.workloadNamespace`; cross-namespace children use deterministic labels and finalizer cleanup rather than invalid owner references.
- `model.id` is a catalog key, not a URL. The catalog maps it to a repository, a permitted revision set, a digest, and an approved quantization. Arbitrary Hugging Face URLs are not accepted.
- `model.revision` must resolve to an immutable commit before creation. Tags and branches are rejected.
- `profile` is an enum. The MVP does not let users specify raw CPU, memory, or GPU counts; a profile maps to a reviewed resource block. This prevents both accidental cluster exhaustion and the class of interview question that begins "so what stops me from requesting 64 GPUs."
- `scaling.minReplicas: 0` is legal and is the interesting case.
- `placement.cachePreference: Required` produces `Unschedulable` rather than a cold start. `Preferred` falls back after `maxColdStartFallbackSeconds`.
- `status.observedGeneration` appears both at the top level and per condition, so a stale condition is distinguishable from a current one.
- Secrets are referenced, never embedded. The MVP serves only public open-weight models and therefore holds no model credentials at all.

### 6.3 Conditions

The original design used six condition types including `Terminal` and `Deleting`. That is not how Kubernetes conditions work. `Deleting` duplicates `metadata.deletionTimestamp`, and `Terminal` describes the retryability of a reason, not an orthogonal state assertion. Ember uses three:

| Condition | Meaning when `True` |
|---|---|
| `Ready` | The verified engine Deployment has completed rollout and its readiness probe is healthy |
| `Progressing` | The controller is actively moving toward the desired state |
| `Degraded` | Something is wrong that the controller cannot resolve by itself |

Retryability is carried by the reason, not by a separate condition. Reasons are stable, machine-readable, and each maps to exactly one UI message template.

**Terminal reasons** (will not succeed without a spec change): `InvalidModel`, `UnsupportedQuantization`, `RevisionNotFound`, `ArtifactDigestMismatch`, `ProfileTooSmallForModel`.

**Retryable reasons** (controller keeps trying with backoff): `InsufficientGPU`, `WeightDownloadFailed`, `ImagePullBackOff`, `NodeNotReady`, `CacheEvictedDuringLoad`.

**Progress reasons**: `Scheduling`, `LoadingWeights`, `WarmingEngine`, `RollingOut`, `ScalingFromZero`.

**Ready reasons**: `EngineServing`, `ScaledToZero`.

`ScaledToZero` is intentionally a `Ready=True` reason rather than a failure. The endpoint is healthy and available; it simply has no warm replica. Modeling it as not-Ready would make every idle endpoint look broken on the dashboard, and would make the `Ready` condition useless as an alerting signal.

### 6.4 `ModelCache` Resource

A second, cluster-scoped CRD tracking weight materialization per node pool.

```yaml
apiVersion: serving.ember.dev/v1alpha1
kind: ModelCache
metadata:
  name: mc-6bd60ea9e062b99a
spec:
  modelID: qwen2.5-7b-instruct-awq
  revision: b25037543e9394b818fdfca67ab2a00ecc7dd641
  digest: sha256:41d12f80b6d62f01e9134f410ab177d907ccb025e41bbb651bd83e8e8304f010
  sizeBytes: 5582381128
  nodePoolSelector:
    ember.dev/gpu: l4
  retentionPolicy: LRUWithFloor
status:
  nodes:
    - name: gke-ember-l4-spot-a3f1
      state: Ready
      materializedAt: "2026-08-15T19:12:44Z"
      lastUsedAt: "2026-08-15T20:47:03Z"
    - name: gke-ember-l4-spot-b7c2
      state: Downloading
      progressBytes: 2103443456
  referencingEndpoints: 2
```

`ModelCache` is cluster-scoped because node cache state is a cluster-level fact shared across tenants. Making it namespaced would either duplicate the same download per tenant or create a cross-namespace reference that ownership garbage collection cannot express.

---

## 7. Reconciliation Algorithm

```go
func (r *EndpointReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
    ep, err := r.get(ctx, req.NamespacedName)
    if apierrors.IsNotFound(err) {
        return done()
    }

    if !ep.DeletionTimestamp.IsZero() {
        return r.reconcileDeletion(ctx, ep)
    }

    if res, err := r.ensureFinalizer(ctx, ep); shouldReturn(res, err) {
        return res, err
    }

    // Terminal validation happens before any resource is created, so an
    // invalid spec never allocates a GPU or triggers a multi-GB download.
    if reason, ok := r.validateAgainstCatalog(ep); !ok {
        return r.markTerminal(ctx, ep, reason)
    }

    r.ensureWorkloadNamespace(ctx, ep)       // quota, netpol, PSA, SA
    cache := r.ensureModelCache(ctx, ep)     // creates or adopts the ModelCache CR

    // Idle handling runs before workload reconciliation so that an idle
    // endpoint does not first get scaled up and then immediately down.
    if r.isIdle(ep) && ep.Spec.Scaling.MinReplicas == 0 {
        return r.scaleToZero(ctx, ep)
    }

    placement, res := r.computePlacement(ctx, ep, cache)
    if res.Requeue {
        return res, nil   // waiting on cache warm-up under Required/Preferred
    }

    r.ensureServingWorkload(ctx, ep, placement)  // Deployment or LeaderWorkerSet
    r.ensureService(ctx, ep)
    r.ensureRoute(ctx, ep)
    r.ensureScaledObject(ctx, ep)                // KEDA, queue-depth trigger

    r.observeRolloutAndReadiness(ctx, ep)
    r.updateStatusIfChanged(ctx, ep)

    return r.requeueForIdleCheckOrPendingState(ep)
}
```

Properties the implementation must preserve:

- **Idempotency.** Every `ensure` step is a server-side apply against a deterministic name derived from the CR UID.
- **Validate before allocate.** Catalog validation precedes namespace creation, GPU allocation, and weight download. A typo in a model ID must cost nothing.
- **No blocking waits.** The controller never sleeps waiting for a download or a rollout. It observes, updates status, and requeues.
- **Bounded retry.** Retryable reasons use exponential backoff with a cap. Terminal reasons update status and stop, without hot-looping.
- **Status discipline.** Status is written only when an externally observable field changes, using the status subresource and optimistic concurrency. Writing status on every reconcile of a stable endpoint produces a write amplification loop that is easy to create and unpleasant to debug.
- **Generation awareness.** Every condition carries `observedGeneration`. A `Ready=True` from two spec revisions ago is not readiness.
- **Deterministic identity.** Child names are `ep-<uid-prefix>`, never derived from user-supplied display strings.

---

## 8. Provisioning Sequence

1. Control API validates the request against the catalog and resolves the revision to an immutable digest.
2. Control API calls the gateway with a short-lived token.
3. Gateway authenticates, injects `ownerID`, and creates the `InferenceEndpoint`.
4. Operator adds a finalizer and validates against the catalog. Invalid specs stop here.
5. Operator creates the UID-derived workload namespace with a strict Ember admission-policy label, `ResourceQuota` including `requests.nvidia.com/gpu`, `LimitRange`, default-deny `NetworkPolicy`, and a ServiceAccount with token automount disabled.
6. Operator creates or adopts the `ModelCache` for this model and revision.
7. `CacheController` sees a `ModelCache` with no `Ready` node, selects a target node, labels it `cache.ember.dev/<hash>=loading`, and creates a prefetch Job pinned to that node.
8. The prefetch Job downloads safetensors shards to `/var/lib/ember/models/<hash>.tmp`, verifies each shard digest, atomically renames the directory, and exits. On success `CacheController` sets the node label to `ready` and updates `ModelCache.status`.
9. `EndpointController` observes a `Ready` node, computes placement, and creates the serving Deployment with `nodeAffinity` toward warm nodes and a toleration for the GPU taint.
10. Scheduler binds the Pod. The init container verifies cache contents. The engine loads weights from the node-local cache.
11. The bounded startup probe and subsequent `/health` readiness probe succeed. The real-GPU smoke workflow separately requires a successful chat completion.
12. Operator observes rollout completion, writes `Ready=True`, `EngineServing`, and the endpoint URL.
13. Operator creates the KEDA `ScaledObject` bound to the endpoint's queue-depth metric.
14. UI receives the transition through bounded SSE.

Steps 7 through 11 are where the project's value concentrates. Steps 1 through 6 are conventional and should be written quickly.

---

## 9. Model Weight Cache and Cache-Aware Placement

This is the headline mechanism.

### 9.1 Problem

Weights for a 7B model at 4-bit are roughly 5 GB; at bf16, 15 GB; a 32B model reaches 60 GB. Pulling from object storage at a realistic 200 MB/s puts cold start between thirty seconds and five minutes. On a spot node pool where nodes come and go, naive placement means most starts are cold. The difference between a warm and a cold start is the difference between a usable endpoint and an unusable one, and no amount of engine tuning recovers it.

### 9.2 Cache Substrate

Node-local NVMe at `/var/lib/ember/models`, mounted into serving Pods as a read-only `hostPath` volume and into prefetch Jobs only at the model-specific writable staging path.

`hostPath` is normally a smell, and both the Baseline and Restricted Pod Security Standards reject it. Ember accepts this deliberately and documents the reasoning: the alternatives are worse for the MVP. A `ReadWriteMany` volume (Filestore, EFS) costs an order of magnitude more than the entire project budget and moves the bottleneck to network storage anyway. A per-Pod `emptyDir` re-downloads on every start, which defeats the entire point. A CSI driver that manages node-local model volumes is the correct production answer and is named in Section 24.

The compromise: every generated workload namespace is labeled `privileged` for Pod Security Admission because PSA has no per-volume exemption mechanism. A cluster-scoped `ValidatingAdmissionPolicy` then admits only operator-shaped serving Pods using the dedicated Ember ServiceAccount, permits exactly `/var/lib/ember/models`, requires serving mounts to be read-only, and retains the remaining Restricted-profile container controls. Prefetch Jobs run only in `ember-system`: a short-lived init container retains only `CHOWN` to make the kubelet-created cache root group-writable, then the downloader runs as UID/GID 65532 with no added capabilities and path-validates the model-specific staging directory before writing. This is weaker and more operationally delicate than Restricted PSA, so the README states it plainly. The security tests in Section 21.4 verify that no other workload Pod can mount the path and that the serving Pod cannot mount it writable.

### 9.3 Cache State as Node Labels

`CacheController` writes `cache.ember.dev/<model-hash>` to each node with values `loading` or `ready`.

Node labels are used rather than a controller-internal map because the scheduler can consume them directly through `nodeAffinity`. Ember does not need a scheduler extender, a scheduling plugin, or a custom scheduler for the MVP; it needs the standard scheduler to see a fact the operator knows. Encoding cache state as node labels is the cheapest correct way to do that, and it survives operator restart because the state lives in the API server rather than in controller memory.

The hash is a short digest of `modelID + revision + quantization`, keeping the label key under the 63-character limit and making cache identity content-addressed rather than name-addressed.

### 9.4 Placement Algorithm

```text
computePlacement(endpoint, cache):
    warm  = nodes where cache.ember.dev/<hash> == ready
            and allocatable nvidia.com/gpu >= profile.gpuCount
    loading = nodes where label == loading

    if warm is non-empty:
        return requiredDuringScheduling nodeAffinity -> warm
               // hard constraint: a warm node exists, use it

    if placement.cachePreference == Required:
        if loading is non-empty:
            status: Progressing / LoadingWeights ; requeue 10s
        else:
            ensurePrefetchJob(cache, pickTargetNode())
            status: Progressing / LoadingWeights ; requeue 10s
        return waiting

    // Preferred
    ensurePrefetchJob(cache, pickTargetNode())
    if elapsedSinceAccepted < maxColdStartFallbackSeconds:
        status: Progressing / LoadingWeights ; requeue 10s
        return waiting

    // Fallback: choose a cold node and warm it explicitly.
    emit event ColdStartFallback
    target = pickColdTargetNode()
    ensurePrefetchJob(cache, target)
    status: Progressing / LoadingWeights ; requeue 10s
    return waiting
```

Three properties worth defending in review:

**Hard affinity once a cache target is selected.** A serving Pod is never asked to download into its read-only cache mount. When a warm node exists, the operator requires that node. After the Preferred deadline, the operator selects a cold node, pins a prefetch Job there, waits for `Ready`, and then uses required affinity to that node. The fallback relaxes which node may be warmed; it does not bypass materialization safety.

**The fallback is time-bounded, not attempt-bounded.** A user waiting on an endpoint cares about wall-clock time. `maxColdStartFallbackSeconds` is the contract: "prefer warm, but never make me wait longer than this to start trying cold."

**Prefetch target selection avoids herding.** `pickTargetNode` prefers a node that is already warm for at least one other model — meaning it has demonstrated NVMe capacity and is not about to be scaled down — and breaks ties by lowest current GPU allocation. Without this, concurrent endpoint creation for different models can serialize all downloads onto one node.

### 9.5 Eviction

`LRUWithFloor`: a `CronJob` runs every ten minutes on each node, and removes cache entries whose `lastUsedAt` exceeds a threshold, oldest first, until free space is above a floor. Entries referenced by a `ModelCache` with `referencingEndpoints > 0` are never evicted. Eviction removes the node label first and the files second, so a Pod can never be scheduled onto a node whose files are mid-deletion.

The reverse ordering is a real race and is worth an explicit test: label removed, Pod scheduled elsewhere, files deleted, no harm. Files deleted, Pod scheduled here, label still says ready, init container fails with `CacheEvictedDuringLoad`, which is retryable and resolves on the next reconcile.

---

## 10. Autoscaling and Scale-to-Zero

### 10.1 Why Not CPU

An inference server saturated on GPU compute shows modest and non-monotonic CPU utilization. Scaling on CPU produces both under- and over-provisioning, and demonstrates that the author has not run the workload. Ember scales on **pending request queue depth**, exported by the engine and scraped by Prometheus.

Queue depth is chosen over TTFT because it is a leading indicator. By the time p95 TTFT degrades, the queue has already been deep for a while and the new replica will take ninety seconds to arrive. Queue depth also composes correctly across replicas, whereas averaging a latency percentile across replicas is not meaningful.

### 10.2 Mechanism

KEDA `ScaledObject` with a Prometheus trigger on `sum(vllm:num_requests_waiting) / count(replicas)`, target `spec.scaling.targetQueueDepth`. Stabilization window of 60 s on scale-down, 0 s on scale-up.

Ember creates and owns the `ScaledObject`; it does not implement its own scaling loop. This is a deliberate scope decision. Writing a bespoke autoscaler would add code without adding signal — everyone can see that a controller can compute `desired = ceil(current * ratio)`. Correctly wiring a custom metric through Prometheus to KEDA to a `scaleTargetRef` on a CR-owned Deployment, and handling the scale-to-zero interaction below, is the part that is actually hard.

Replica field ownership is explicit: KEDA owns ordinary `1..maxReplicas` scaling through the Deployment scale subresource. The operator does not continuously apply `spec.replicas`. It may write the scale subresource only for idle transition to zero and gateway-triggered activation from zero to one, recording the reason in endpoint status. This prevents server-side apply from fighting the autoscaler.

Before either operator-owned scale write, the operator pauses KEDA with `autoscaling.keda.sh/paused: "true"`. It keeps KEDA paused through the zero-to-one readiness path and removes the annotation only after activation completes. It deliberately does not use `paused-replicas`: that variant asks KEDA to write the replica count itself and would create two concurrent writers during Ember's explicit transitions.

### 10.3 Scale-to-Zero and Cold Start

When `time.Since(status.lastActivityTime) > idleTimeoutSeconds` and `minReplicas == 0`, the operator scales the Deployment to zero, sets `Ready=True` with reason `ScaledToZero`, and keeps the Service, route, and `ModelCache` reference intact.

`lastActivityTime` is updated by the gateway, not by the operator, because the gateway is the only component on the request path. The gateway patches the CR status at most once per thirty seconds per endpoint to avoid turning a busy endpoint into an API server write storm.

On the next request the gateway finds zero endpoints behind the Service. It then:

1. Patches `lastActivityTime` and sets an activation annotation.
2. Returns HTTP 503 with `Retry-After: 5` and a JSON body carrying the endpoint's current condition, so the UI can display a real progress state instead of an error.
3. The operator observes the annotation, scales to one, and the normal readiness path runs.

Holding the request open until the replica is ready would be friendlier, and is explicitly rejected: it requires the gateway to buffer connections for up to two minutes, which turns a stateless proxy into a stateful one and creates a new class of failure the project does not need. The 503-with-progress pattern is honest and is what the UI is designed around.

Cluster-autoscaler with a spot GPU node pool scaled to zero adds a second layer: if no GPU node exists at all, the Pod is `Unschedulable`, the autoscaler provisions a node in two to four minutes, and the cache is guaranteed cold. The UI must distinguish `ScalingFromZero` (replica coming) from `ProvisioningNode` (machine coming), because the expected wait differs by an order of magnitude.

---

## 11. Multi-GPU Serving

A `tp2` profile runs tensor-parallel across two GPUs.

**Single-node TP (MVP).** One Pod requesting `nvidia.com/gpu: 2`. The Kubernetes scheduler handles this atomically — a Pod is scheduled or it is not — so no gang scheduling is required. This is the MVP path and it is honest to say so.

**Multi-node TP (stretch, Section 23).** Two Pods that must both be scheduled or neither, because a half-scheduled TP group holds one GPU hostage indefinitely. This requires LeaderWorkerSet for the topology and a gang-scheduling admission path (Kueue or Volcano) for the all-or-nothing guarantee. The failure mode when this is done wrong — two endpoints each holding one of two GPUs and both waiting forever — is a good demonstration and a good interview story, and the test suite in Section 21.3 deliberately reproduces it before the fix.

The MVP ships single-node TP and documents multi-node as unimplemented, rather than shipping a broken version of it.

---

## 12. GPU Scheduling and Resource Governance

- GPU nodes carry taint `nvidia.com/gpu=present:NoSchedule` and label `ember.dev/gpu=l4`. Only Ember serving and prefetch Pods tolerate the taint, so no system workload occupies a GPU node's CPU capacity in a way that blocks a serving Pod.
- Every per-endpoint workload namespace has `ResourceQuota` on `requests.nvidia.com/gpu`, `requests.memory`, Jobs, and Pods.
- Because `InferenceEndpoint` objects live in `ember-system`, per-owner endpoint count is enforced independently by the gateway and rechecked by the operator before any allocation. This prevents a caller with direct CR access from bypassing the product limit and producing unbounded prefetch Jobs.
- `LimitRange` supplies defaults so that a Pod created outside the operator path still cannot request unbounded memory.
- `PriorityClass`: prefetch Jobs run at a lower priority than serving Pods, so a download never preempts serving. Serving Pods are not preemptible by each other.
- The operator reads `node.status.allocatable["nvidia.com/gpu"]` when producing the `InsufficientGPU` message, so the error names actual numbers rather than saying "no capacity."

---

## 13. Deletion, Idle, and Reclamation

Reclamation correctness is a primary feature. A leaked GPU costs real money per hour and is the failure mode a reviewer of an AI infrastructure project will look for first.

### 13.1 Why a Finalizer Is Required

The workload namespace is a cluster-scoped object. A namespaced `InferenceEndpoint` in `ember-system` cannot own it through `ownerReferences`: Kubernetes garbage collection does not permit a namespaced owner for a cluster-scoped dependent, and such a reference is silently treated as invalid rather than rejected. The same applies to the cluster-scoped `ModelCache` and to node labels, which are not owned objects at all.

Therefore the finalizer is not a stylistic choice; it is the only mechanism available. Stating this in the document is worth more than the finalizer code itself.

Cross-namespace Deployment, Service, HTTPRoute, and ScaledObject objects cannot carry a valid owner reference to the `InferenceEndpoint` in `ember-system`. They instead carry the immutable endpoint UID label. The finalizer deletes and verifies every labeled child plus the workload namespace. This is more work than same-namespace garbage collection, but it is the correct consequence of keeping public CR creation in a fixed control namespace.

### 13.2 Finalizer Flow

`serving.ember.dev/endpoint-cleanup`:

1. Set `Progressing=True`, reason `Terminating`.
2. Scale the workload to zero and wait for Pods to be gone, so GPUs are released before slower cleanup steps run.
3. Delete namespace-scoped children (or rely on GC and verify).
4. Decrement `ModelCache.status.referencingEndpoints`. If it reaches zero, mark the cache entries evictable — but do not delete them. The next endpoint for the same model should still find a warm cache.
5. Delete the endpoint workload namespace.
6. Requeue until the namespace is actually absent.
7. Remove the finalizer.

Step 2 before step 3 is the ordering that matters. Deleting the namespace first also releases GPUs, but the operator loses the ability to observe and report the release.

### 13.3 Orphan Sweeper

A low-frequency `CronJob` compares three things: namespaces labeled `ember.dev/managed=true`, node labels under `cache.ember.dev/`, and live `InferenceEndpoint` UIDs. It reports orphans as a metric and, after a conservative grace period, removes them.

Automatic deletion is **disabled by default** and enabled only after the security tests prove the selector cannot match unrelated objects. A sweeper that deletes namespaces is one label-selector bug away from being the worst component in the system.

---

## 14. Isolation and Security

### 14.1 Threat Model

Ember's threat model is substantially simpler than a general code-execution platform's, and the document should claim that reduction rather than obscure it. Users never supply code, container images, engine arguments, or model URLs. They select a catalog key and a profile. The primary risks are therefore supply chain, resource exhaustion, and cross-tenant access — not arbitrary code execution.

Explicitly outside the security claim:

- Kernel-level isolation between tenants sharing a GPU node.
- GPU memory side channels between co-resident workloads. The MVP allocates whole GPUs, which sidesteps but does not solve this.
- Defense against a malicious model artifact that is itself a valid safetensors file.
- Denial of service by a tenant issuing expensive long-context requests.

### 14.2 Supply Chain

- **Safetensors only.** PyTorch `.bin` and `.pt` checkpoints are pickle archives and deserializing one executes arbitrary Python. The catalog rejects any model whose artifacts are not safetensors, and the prefetch Job refuses to materialize a file that fails format validation. This single constraint eliminates the largest realistic attack surface in a model-serving system and is worth a paragraph in the README.
- Every artifact is verified against a digest recorded in the catalog at review time, not fetched alongside the download.
- Engine images are pinned by digest, not tag.
- The prefetch Job's egress is restricted to the model registry and DNS. Serving Pods have no general egress at all.

### 14.3 Workload Controls

- Workload namespaces use `privileged` Pod Security Admission only because PSA cannot exempt a single `hostPath`. A `ValidatingAdmissionPolicy` restores the intended narrow shape: exact ServiceAccount and cache path, read-only serving mount, bounded writable prefetch staging path, non-root execution, no privilege escalation, dropped capabilities, seccomp, and read-only root filesystems.
- Containers run non-root, no privilege escalation, all capabilities dropped, `seccompProfile: RuntimeDefault`, read-only root filesystem.
- ServiceAccount with `automountServiceAccountToken: false`. A serving Pod has no path to the Kubernetes API.
- Default-deny ingress and egress `NetworkPolicy`, with ingress permitted only from the gateway and Prometheus.
- The NetworkPolicy claim is verified in tests against a policy-enforcing CNI. GKE Dataplane V2 enforces it; a kind cluster without a policy-capable CNI does not, and a test suite that passes on kind proves nothing here.

### 14.4 Control-Plane Controls

- Browsers never receive Kubernetes credentials; the Replit service never receives cluster-admin.
- Gateway tokens are short-lived and audience-scoped.
- Ownership is enforced at the gateway on every read, mutation, log stream, and inference proxy call, independently of anything the UI sends.
- Model IDs, profiles, replica bounds, and idle timeouts are validated twice: at the control API and again at the gateway/CRD admission boundary.
- Engine logs are size-bounded, redacted for known secret patterns, and never cross an ownership boundary.
- Audit entries record actor, action, endpoint UID, request ID, result, and timestamp.

---

## 15. Networking and Routing

A single host, `ember.example.dev`, with path-based routing: `/v1/endpoints/{id}/...` proxied by the gateway to the endpoint's ClusterIP Service.

The original design used wildcard DNS with a wildcard certificate per workspace subdomain. That is dropped. Inference endpoints are API surfaces called with an explicit base URL, not web applications with root-relative assets, so the argument for host-based routing does not apply. Path routing removes a DNS provider dependency, a wildcard certificate, and roughly a day of work that produces no reviewable artifact.

Gateway API `HTTPRoute` is used rather than Ingress, because the MVP needs weighted backend splitting for canary rollout (Section 16) and Ingress cannot express it without controller-specific annotations.

Streaming responses pass through as SSE. The gateway must not buffer them; a buffering proxy converts a streaming endpoint into a batch one and makes the TTFT measurement meaningless.

---

## 16. Model Revision Rollout

Changing `spec.model.revision` triggers a rollout that must not drop in-flight requests and must not double GPU consumption when there is only one GPU.

The operator:

1. Ensures the new revision's `ModelCache` is `Ready` on some node **before** touching the serving workload. Rolling out to a cold cache means the endpoint is down for the duration of the download.
2. If cluster GPU headroom permits, creates a second Deployment at the new revision, waits for readiness, shifts the `HTTPRoute` weight, then removes the old one.
3. If headroom does not permit, sets `Degraded=True` with reason `InsufficientGPUForRollout` and offers the user an explicit in-place replacement that accepts downtime.

Case 3 is the common case on a two-node demo cluster and it is the more interesting one. Silently doing an in-place restart and calling it a rollout would be dishonest; surfacing the trade-off is the correct control-plane behavior.

---

## 17. Observability

Every request and reconcile is correlated by `request_id` and immutable `endpoint_uid`.

### 17.1 Metrics

Control-plane:

- `ember_reconcile_total{controller,result}`
- `ember_reconcile_duration_seconds{controller}`
- `ember_endpoint_phase_transitions_total{from,to,reason}`
- `ember_endpoints_active{phase}`
- `ember_gateway_requests_total{op,status}`

Inference lifecycle — these are the ones that make it an AI infrastructure project:

- `ember_endpoint_ready_seconds{cache_state}` — histogram, labeled `hit` or `miss`. The whole story of Section 9 is one query against this metric.
- `ember_scale_from_zero_seconds{node_provisioned}` — distinguishes replica cold start from node provisioning.
- `ember_ttft_seconds{endpoint}` — measured at the gateway, not reported by the engine, so it includes proxy and queueing.
- `ember_model_download_bytes_total{model}` and `ember_model_download_seconds{model}`
- `ember_cache_hit_total{result}` and `ember_cache_bytes_on_node{node}`
- `ember_cache_evictions_total{reason}`
- `ember_gpu_seconds_total{endpoint}` — allocated GPU time, the cost metric
- `ember_gpu_idle_seconds_total{endpoint}` — allocated but no requests, the waste metric

The last two pairs let the dashboard answer "what did this endpoint cost and how much of it was wasted," which is the question an infrastructure team actually asks.

### 17.2 Structured Logs

```json
{
  "level": "info",
  "controller": "endpoint",
  "endpoint_uid": "4d71...",
  "reconcile_id": "rec_9012",
  "generation": 3,
  "action": "compute_placement",
  "cache_state": "hit",
  "target_node": "gke-ember-l4-spot-a3f1",
  "result": "affinity_required",
  "duration_ms": 7
}
```

Engine logs are a separate data class: bounded, redacted, ownership-scoped, never mixed with control-plane logs.

### 17.3 Dashboard

- Endpoint lifecycle timeline with per-phase durations, so the cold-start breakdown is visible: scheduling, download, load, warm.
- Cold versus warm start latency distributions, side by side. This is the single most valuable chart in the project.
- Queue depth and replica count over time, overlaid, showing the autoscaler reacting.
- GPU allocation and idle time per endpoint.
- Node cache contents and free space.
- Current conditions and reason codes with the full message and evidence.

The implemented Kind product surface also includes:

- Fleet and endpoint views with History API deep links.
- An endpoint creation drawer constrained by the server-side catalog and profile limits.
- OpenAI-compatible chat with SSE token streaming and explicit scale-from-zero retry state.
- A concurrent browser Load Lab that drives the real request path and lets KEDA behavior appear in the same metrics view.
- An owner-scoped Kubernetes Inspector for generated Deployments, Services, quotas, Pods, NetworkPolicies, HPA, ScaledObjects, and namespace events.
- Bounded/redacted engine logs and append-only product audit history.

Runtime phase and conditions come from the CR, metric history comes from Prometheus, topology comes from the Kubernetes API, and audit history comes from Postgres. Browser-observed TTFT and the illustrative GPU allocation meter are labeled as client observations rather than real GPU benchmark or billing data.

---

## 18. Failure Handling

| Failure | Detection | Response |
|---|---|---|
| Model not in catalog / bad revision | Admission and controller validation | `Degraded`, terminal reason, nothing allocated |
| Non-safetensors artifact | Prefetch Job format check | Terminal `UnsupportedQuantization`, cache marked poisoned |
| Digest mismatch | Prefetch Job verification | Terminal `ArtifactDigestMismatch`, entry deleted, alert |
| Weight download fails | Job backoff limit | Retryable, exponential backoff, `WeightDownloadFailed` after 3 attempts |
| No allocatable GPU | Pod `Unschedulable` > 60 s | `Degraded`, `InsufficientGPU` with real node numbers |
| Model too large for profile | Container exit 137 during load | Terminal `OOMDuringLoad` with the size arithmetic in the message |
| Engine starts but never becomes healthy | Bounded startup and `/health` readiness probes | Kubernetes restarts the process; rollout remains `Progressing`, with logs retained for inspection |
| Spot node preempted | Node `NotReady` / Pod deleted | Deployment reschedules; if the new node is cold, report `ColdStartFallback`, do not silently absorb the latency |
| Cache evicted during load | Init container check fails | Retryable `CacheEvictedDuringLoad`, prefetch re-triggered |
| Operator restart mid-provision | Watch re-list | Idempotent reconcile resumes; no duplicate Job or Deployment |
| Gateway restart | Stateless; k8s is source of truth | Client retries; `lastActivityTime` may lose up to 30 s of resolution, documented |
| Replit API restart | Postgres + CR status | Presentation reconstructed from gateway |
| Namespace stuck terminating | Finalizer timeout metric | Keep CR terminating, alert, never report a false deletion |
| Rollout with no headroom | Quota check before rollout | `InsufficientGPUForRollout`, offer explicit in-place path |

---

## 19. External API

```http
POST   /api/v1/session
GET    /api/v1/session
DELETE /api/v1/session
POST   /api/v1/endpoints
GET    /api/v1/endpoints
GET    /api/v1/endpoints/{id}
DELETE /api/v1/endpoints/{id}
GET    /api/v1/endpoints/{id}/events
GET    /api/v1/endpoints/{id}/logs?tail=200
GET    /api/v1/endpoints/{id}/stream
GET    /api/v1/endpoints/{id}/metrics?window=900&step=5
GET    /api/v1/endpoints/{id}/inspect
POST   /api/v1/endpoints/{id}/v1/chat/completions    # OpenAI-compatible
GET    /api/v1/catalog/models
```

Create request:

```json
{
  "modelID": "qwen2.5-7b-instruct-awq",
  "profile": "standard",
  "idleTimeoutSeconds": 900,
  "cachePreference": "Preferred"
}
```

The external API accepts product-level identifiers, never raw Kubernetes manifests and never engine flags. The OpenAI-compatible path exists so a reviewer can point any existing client at the endpoint, which is a stronger demonstration than a bespoke chat box.

`POST /api/v1/endpoints` requires an `Idempotency-Key` header. `events` returns the append-only product audit history; `stream` proxies authoritative CR status changes. Endpoint rows are soft-deleted only after the gateway confirms the CR is absent, so model, ownership, timing, and audit metadata remain available after Kubernetes reclamation.

`metrics` accepts only bounded `window` and `step` values and maps them to fixed server-side Prometheus queries scoped by immutable endpoint UID; callers cannot submit PromQL. `inspect` performs fixed read-only queries inside the endpoint's generated workload namespace after the same ownership check. Neither telemetry polling route appends audit records, avoiding five-second polling noise in the product action history.

---

## 20. Data Model and Source of Truth

| Data | Source of truth | Rationale |
|---|---|---|
| Desired serving state | `InferenceEndpoint.spec` | Declarative, watched |
| Observed workload state | Kubernetes child resources | Actual runtime truth |
| User-visible lifecycle | `.status.conditions` | Derived from observation |
| Node cache state | Node labels + `ModelCache.status` | Must survive operator restart; scheduler must read it |
| Last activity | `.status.lastActivityTime`, written by gateway | Only the gateway sees the request path |
| Model catalog | Git-versioned config, loaded as ConfigMap | Reviewed artifact, not user input |
| Demo session | Postgres, keyed by a hash of an opaque cookie token | Survives web restarts without exposing gateway credentials |
| Ownership | Control API DB + gateway-injected `ownerID` | Never trusted from CR client input |
| Presentation metadata and idempotency | Postgres | Must survive API restarts and CR deletion |
| Audit history | Append-only Postgres | Must outlive the CR |
| Metrics history | Prometheus | Not in the CR |

The database must never independently claim an endpoint is Ready or Deleted. Those are projections of cluster state and confirmed resource absence.

---

## 21. Testing Strategy

### 21.1 Unit

- Catalog validation and profile resolution.
- Deterministic child object generation, including affinity term construction for each placement branch.
- Condition and reason transition table, exhaustively.
- Cache hash computation and label-length bounds.
- Idle computation across clock skew and missing `lastActivityTime`.
- Eviction ordering: label before files.

### 21.2 Controller (`envtest`)

- First reconcile creates exactly the expected object set.
- Repeated reconcile is a no-op; assert zero writes on a stable endpoint.
- Terminal validation creates no namespace, no Job, no Deployment.
- Serving Deployment is not created before `ModelCache` reports a `Ready` node under `Required`.
- `Preferred` falls back to soft affinity after the configured deadline, not before.
- Status carries the correct `observedGeneration` after a spec bump.
- Deletion waits for Pod disappearance before removing the finalizer.
- Conflicting concurrent updates retry without lost writes.

### 21.3 Integration on kind with a simulated GPU

kind plus an in-repository fake GPU device plugin, which advertises `nvidia.com/gpu`, plus a mock engine image that sleeps proportionally to a configured weight size, exposes synthetic queue metrics, and serves an OpenAI-compatible surface. Both components are built from reviewed repository source; the test path does not download or execute an unreviewed GPU operator or installation script.

This tier runs in CI on every commit and covers:

- Full lifecycle: create, warm, ready, autoscale, idle, zero, reactivate, delete.
- Cache-aware placement: warm one node, create an endpoint, assert it landed there.
- Cache miss: no warm node, assert prefetch Job creation and correct placement afterward.
- `Required` with no warm node and no capacity: assert it waits and does not fall back.
- Two endpoints, one GPU: assert one is Ready and one reports `InsufficientGPU` with correct numbers.
- Eviction race: delete cache files without the label, assert `CacheEvictedDuringLoad` and recovery.
- Operator restart injected during download and during rollout.
- Delete a child Deployment; assert reconciliation recreates it.
- Multi-node TP deadlock reproduction: two `tp2` endpoints on three GPUs without gang scheduling, assert the deadlock, then assert the gang-scheduled version does not deadlock.
- Advance idle clocks; assert every endpoint reaches zero and every object disappears.
- Build and serve the React SPA from the Control API's same origin.
- Create an opaque browser session and validate catalog-constrained, idempotent endpoint creation.
- Read owner-scoped metrics and Kubernetes inspection data without exposing a kubeconfig or Gateway JWT to the browser.
- Exercise ordinary and SSE chat, browser-equivalent concurrent load, bounded logs, append-only audit history, API/Postgres restart recovery, and retained deleted views.

The mock engine is what makes this tier possible, and building it early is the highest-leverage decision in the project. Everything except real GPU throughput can be tested for free.

### 21.4 Security

- Assert the admission policy rejects a Pod outside the Ember ServiceAccount that attempts to mount the cache `hostPath`.
- Assert the serving Pod's cache mount is read-only.
- Assert serving Pods cannot reach the Kubernetes API.
- Assert one owner cannot read, delete, stream logs from, or send inference to another's endpoint.
- Assert default-deny egress on serving Pods and restricted egress on prefetch Jobs, on a policy-enforcing CNI.
- Assert the gateway ServiceAccount cannot create Pods or read Secrets.
- Assert a non-safetensors artifact is rejected before materialization.
- Assert a digest mismatch is rejected and reported.

### 21.5 Real GPU validation

Run once per phase on GKE with L4 spot nodes, not in CI:

- Real cold and warm start latency for two model sizes, ten samples each, p50 and p95 reported.
- Real TTFT and throughput at concurrency 1, 4, and 16.
- Real scale-from-zero including a node provisioning event.
- Real spot preemption, forced by draining the node, with the recovery path observed.
- One `tp2` endpoint on a 2×L4 node.

Everything reported in the README comes from this tier, with the cluster, GPU, model, quantization, prompt shape, and sample count stated inline. Numbers from the mock tier are never presented as performance results.

---

## 22. Infrastructure and Cost Plan

The project is designed so that development costs nothing and only validation costs money.

| Tier | Environment | Cost |
|---|---|---|
| Development, CI | kind + in-repository fake GPU device plugin + mock engine | $0 |
| Always-on demo control plane | Single small CPU VM (Hetzner/DO) running k3s server, operator, gateway, Prometheus | ~$10/month |
| GPU validation and demo recording | GKE, L4 spot node pool, scaled from zero | ~$0.30–0.40/hr all-in |

**Budget.** A validation session is six to eight hours including setup and recording. Four sessions plus one two-node session lands at roughly $40–60. A hard ceiling of $150 applies; exceeding it means logic is being debugged on GPUs, which is the wrong tier. If unused new-account credit is available on GCP it likely covers the whole project.

**Two practical constraints.** GCP GPU quota defaults to zero and must be requested in advance, with a one-to-two day turnaround. And a forgotten instance is the largest real financial risk in a project like this, so a budget alert and a hard shutdown timer are part of the deployment scripts, not an afterthought.

**Always-on demo behavior.** The control plane stays up permanently; the GPU node pool sits at zero. A visitor creating an endpoint sees `Progressing / ProvisioningNode` and, absent a running node pool, eventually `InsufficientGPU` with an accurate message. This is not a degraded demo — it is the scale-from-zero path with the node tier disabled, and the UI states exactly that. A two-minute recording at the top of the README covers the GPU-backed portion.

RunPod is explicitly excluded as an option: its Pods and Instant Clusters are containerized environments that do not support Kubernetes, and its bare-metal tier requires a multi-month commitment.

---

## 23. Delivery Plan

Scoped against a real constraint: interview season begins shortly, so the plan front-loads the parts that carry signal and treats everything after Phase 3 as optional.

### Phase 1 — Operator core

Scaffold the Go operator, `v1alpha1` CRDs, catalog config. Reconcile namespace, quota, policy, ServiceAccount, Deployment, Service. Conditions, `observedGeneration`, finalizer, idle detection. Mock engine image. Full lifecycle demonstrated through `kubectl` on kind with fake GPUs.

**Exit:** one `InferenceEndpoint` YAML produces a reachable mock endpoint; deletion reclaims everything; `envtest` suite green.

### Phase 2 — Cache and placement

`ModelCache` CRD and `CacheController`. Prefetch Job, digest verification, safetensors enforcement, node labels, atomic materialization. Placement algorithm with all three branches. Eviction with correct ordering. The integration tests in 21.3 covering cache behavior.

**Exit:** warm placement is provably chosen over cold; `Required` waits; `Preferred` falls back on deadline; eviction race handled.

### Phase 3 — Autoscaling, zero, and real GPUs

KEDA integration on queue depth. Scale-to-zero, gateway activation path, `lastActivityTime`. Gateway with short-lived JWT authentication, ownership enforcement, inference proxy, bounded logs. Swap the mock engine for real vLLM. First GKE validation session.

**Exit:** on GKE, an endpoint serves real tokens, autoscales under load, scales to zero, cold-starts on the next request, and every number in Section 3.2 has a measured value.

### Phase 4 — Product surface and evidence

Replit UI: lifecycle timeline, conditions with evidence, chat box, load generator, cold-versus-warm latency chart, GPU cost and idle panel. Prometheus dashboards. Full failure-injection and security suites. README with reproducible commands and honest scoping. Two-minute demo recording.

**Exit:** a reviewer completes the whole flow from one URL, and every claim maps to a script they could run.

### Phase 5 — Optional, in priority order

1. MIG partitioning or time-slicing for fractional GPU allocation. This is the highest-value extension and is gated only on hardware access; an A100 or H100 session for a single afternoon would cover it.
2. Multi-node TP via LeaderWorkerSet plus Kueue gang scheduling, with the deadlock reproduction as the motivating test.
3. Prefix-aware routing across replicas to exploit KV cache locality.
4. A CSI driver for node-local model volumes, replacing the `hostPath` compromise in 9.2.

Phase 5 items are listed in the README as "designed, not built," with the design sketch included. An honest unbuilt roadmap with reasoning is a stronger signal than a half-working implementation.

---

## 24. Alternatives Considered

### 24.1 Use KServe, llm-d, or AIBrix Instead

Correct answer for production, and the README says so in the first paragraph. Those projects implement model caching, autoscaling, and routing more completely than this one will. Ember exists to demonstrate that the author can build the mechanism, not configure it. The honest framing — "in production I would deploy llm-d; I built this to understand what it is doing" — is more credible than pretending the ecosystem does not exist, and it invites exactly the conversation the project is meant to start.

### 24.2 Create Kubernetes Objects Directly From the Web API

Simpler at first, but the public API becomes responsible for imperative multi-resource orchestration, restart recovery, and drift correction. An operator keeps desired state in the API server, resumes after failure by construction, and makes lifecycle semantics explicit. Operator preferred.

### 24.3 Skip the Cache and Pull Weights Every Start

Removes Section 9 entirely, which removes most of the project's differentiation. It would also make every measured cold start three to five minutes, which is a fair description of a system nobody would use.

### 24.4 Use ReadWriteMany Shared Storage Instead of Node-Local Cache

Simpler placement — any node can mount it, so cache-aware scheduling becomes unnecessary. Rejected on two grounds: managed RWX storage exceeds the entire project budget by an order of magnitude, and it relocates the bottleneck to network bandwidth shared across all replicas, which is worse than local NVMe for exactly the loads that matter. The trade-off is real, though, and a production design with modest scale might reasonably choose it.

### 24.5 Write a Custom Scheduler or Scheduler Extender

The most direct way to express cache-aware placement. Rejected because node labels plus `nodeAffinity` achieve the same placement for this problem using the stock scheduler, and a custom scheduler introduces an operational component that must be highly available, correct under contention, and independently upgradeable. Worth revisiting only when placement needs to consider signals the affinity API cannot express, such as bin-packing across the cache and GPU dimensions jointly.

### 24.6 Scale on GPU Utilization Instead of Queue Depth

GPU utilization as reported by DCGM is high whenever any kernel is running and does not distinguish a saturated server from a lightly loaded one running a large batch. Queue depth measures the thing that actually degrades user latency.

### 24.7 Use Cloud Run GPU or a Serverless Inference Provider

Reasonable for shipping a product; it would provide scale-to-zero for free. It also removes the CRD, controller, scheduling, cache, quota, and reclamation problems this project exists to demonstrate.

### 24.8 Run Kubernetes Inside Replit

Nested orchestration requires privileged containers, does not resemble a real deployment, and cannot access GPUs. Replit hosts the product surface; the cluster stays a separately managed execution plane.

---

## 25. Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Scope expands into a KServe clone | Never reaches a working demo | Phase 5 is explicitly unbuilt; MIG, multi-node TP, and prefix routing are documented, not implemented |
| Real GPU debugging burns budget | Cost overrun, slow iteration | Mock engine + fake GPU operator for all logic; GPUs only for measurement; $150 hard ceiling |
| GCP GPU quota not granted in time | Phase 3 blocked | Request quota during Phase 1, before it is needed |
| Forgotten GPU instance | Unbounded cost | Budget alerts, shutdown timer in deploy scripts, teardown as part of the session checklist |
| `hostPath` cache weakens the security story | Undermines an otherwise clean threat model | Narrow exemption, read-only, tested; documented as the CSI-driver gap |
| Cache-aware placement is subtly wrong | The project's headline feature is broken | Three explicit placement branches, each with a dedicated integration test; eviction ordering tested |
| Spot preemption during demo recording | Failed recording | Record with on-demand nodes; test preemption separately and deliberately |
| Cold-start numbers look bad | Weak-seeming results | Report them anyway, with the warm comparison; the delta is the result, not the absolute |
| NetworkPolicy configured but unenforced | False security claim | Verify on GKE Dataplane V2, not on kind |
| Portfolio metrics look cherry-picked | Credibility loss | Publish cluster spec, GPU, model, quantization, prompt shape, sample count, and the raw script |
| Reviewer asks "why not just use llm-d" | Project looks naive | Section 24.1 answers it in the README's first section |

---

## 26. Success Criteria

The project is portfolio-ready when all of the following hold:

- A public URL serves the application and its always-on control plane.
- Creating an endpoint produces real Kubernetes objects with correct ownership and quota.
- Cache-aware placement is demonstrably correct: warm nodes are chosen, `Required` waits, `Preferred` falls back on deadline, and the integration tests prove all three.
- A real model serves real tokens through an OpenAI-compatible route on a real GPU.
- The endpoint autoscales on queue depth under generated load and returns to zero on idle, with the GPU visibly released.
- Cold and warm start latencies are measured, reported side by side, and the difference is attributable to the cache.
- The operator recovers from restart during download, during rollout, and during deletion.
- Security tests demonstrate admission-restricted Pods, read-only serving cache, bounded GPU quota, narrow RBAC, cross-owner denial, and safetensors enforcement.
- Zero leaked GPUs, namespaces, node labels, or cache entries after the full suite.
- The README states the threat model, the unbuilt roadmap, and the relationship to KServe and llm-d, without overclaiming any of them.

---

## 27. Questions for Technical Review

1. Is `ModelCache` correctly scoped as a cluster-scoped CRD, or should node cache state live entirely in node labels with no CRD at all?
2. Are node labels plus `nodeAffinity` sufficient for cache-aware placement, or does the fallback branch justify a scheduler plugin sooner than Section 24.5 argues?
3. Is the `Required` / `Preferred` + time-bounded fallback the right user-facing contract, or should the fallback be automatic and invisible?
4. Is `ScaledToZero` as a `Ready=True` reason the right modeling choice, or does it overload `Ready` in a way that breaks alerting?
5. Is the 503-with-`Retry-After` activation path acceptable, or should the gateway hold the connection despite the statefulness it introduces?
6. Does the `hostPath` cache exemption undermine the security claims enough that a CSI driver should move into the MVP?
7. Is the eviction ordering (label first, files second) sufficient, or is there a race the `CacheEvictedDuringLoad` path does not cover?
8. Should single-node `tp2` ship in the MVP at all, given that multi-node TP — the interesting case — is deferred?
9. Which of the Section 3.2 targets is most likely to be missed, and would missing it invalidate the project's thesis?
10. What should be cut to get Phases 1 through 3 done faster?

---

## 28. References

- Kubernetes, [Controllers](https://kubernetes.io/docs/concepts/architecture/controller/)
- Kubernetes, [Custom Resources](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/)
- Kubernetes, [Operator Pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)
- Kubernetes, [Finalizers](https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/)
- Kubernetes, [Owners and Dependents](https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/)
- Kubernetes, [Schedule GPUs](https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/)
- Kubernetes, [Assigning Pods to Nodes](https://kubernetes.io/docs/concepts/scheduling-eviction/assign-pod-node/)
- Kubernetes, [Taints and Tolerations](https://kubernetes.io/docs/concepts/scheduling-eviction/taint-and-toleration/)
- Kubernetes, [Resource Quotas](https://kubernetes.io/docs/concepts/policy/resource-quotas/)
- Kubernetes, [Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
- Kubernetes, [Network Policies](https://kubernetes.io/docs/concepts/services-networking/network-policies/)
- Kubernetes, [Gateway API](https://gateway-api.sigs.k8s.io/)
- Kubernetes SIGs, [LeaderWorkerSet](https://github.com/kubernetes-sigs/lws)
- Kubernetes SIGs, [Kueue](https://kueue.sigs.k8s.io/)
- KEDA, [Prometheus Scaler](https://keda.sh/docs/latest/scalers/prometheus/)
- vLLM, [OpenAI-Compatible Server](https://docs.vllm.ai/en/latest/serving/openai_compatible_server.html)
- Hugging Face, [Safetensors](https://huggingface.co/docs/safetensors/index)
- Kubernetes, [Device Plugins](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/)
