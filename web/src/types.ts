export type EndpointPhase =
  | "Creating"
  | "Pending"
  | "Progressing"
  | "Ready"
  | "Degraded"
  | "Deleting"
  | "Deleted";

export interface Session {
  id: string;
  ownerID: string;
  createdAt: string;
  expiresAt: string;
}

export interface CatalogModel {
  id: string;
  revision: string;
  digest: string;
  sizeBytes: number;
  engineImage: string;
  allowedProfiles: string[];
}

export interface CatalogProfile {
  name: string;
  gpuCount: number;
  cpuRequest: string;
  memoryRequest: string;
  memoryLimit: string;
  shmSize: string;
}

export interface Catalog {
  models: CatalogModel[];
  profiles: CatalogProfile[];
}

export interface EndpointCondition {
  type: string;
  status: string;
  reason: string;
  message: string;
  observedGeneration?: number;
  lastTransitionTime: string;
}

export interface EndpointRuntime {
  uid?: string;
  phase: EndpointPhase;
  observedGeneration?: number;
  replicas?: {
    desired?: number;
    ready?: number;
  };
  placement?: {
    node?: string;
    cacheState?: string;
  };
  model?: {
    resolvedDigest?: string;
    sizeBytes?: number;
  };
  lastActivityTime?: string;
  conditions?: EndpointCondition[];
}

export interface Endpoint {
  id: string;
  displayName: string;
  modelID: string;
  revision: string;
  profile: string;
  minReplicas: number;
  maxReplicas: number;
  targetQueueDepth: number;
  idleTimeoutSeconds: number;
  cachePreference: "Preferred" | "Required";
  maxColdStartFallbackSeconds: number;
  createdAt: string;
  provisionedAt?: string;
  deletionRequestedAt?: string;
  deletedAt?: string;
  inferencePath: string;
  runtime?: EndpointRuntime;
  runtimeError?: {
    code: string;
    message: string;
  };
}

export interface CreateEndpointInput {
  displayName: string;
  modelID: string;
  profile: string;
  minReplicas: number;
  maxReplicas: number;
  targetQueueDepth: number;
  idleTimeoutSeconds: number;
  cachePreference: "Preferred" | "Required";
  maxColdStartFallbackSeconds: number;
}

export interface AuditEvent {
  id: number;
  actor: string;
  action: string;
  endpointID?: string;
  endpointUID?: string;
  requestID: string;
  result: string;
  details: Record<string, unknown>;
  createdAt: string;
}

export interface InspectionResource {
  kind: string;
  name: string;
  namespace: string;
  status: string;
  summary: string;
  details?: Record<string, string>;
}

export interface InspectionPod {
  name: string;
  phase: string;
  ready: boolean;
  node?: string;
  image?: string;
  restartCount: number;
  requestedGPUs: number;
  startedAt?: string;
}

export interface InspectionNetworkPolicy {
  name: string;
  policyTypes: string[];
  ingressRules: number;
  egressRules: number;
}

export interface InspectionEvent {
  type: string;
  reason: string;
  message: string;
  count: number;
  objectKind: string;
  objectName: string;
  lastSeen: string;
}

export interface InspectionSecurity {
  name: string;
  state: "pass" | "fail" | "warning" | "unknown" | "pending";
  evidence: string;
}

export interface EndpointInspection {
  observedAt: string;
  endpointUID: string;
  namespace: string;
  resources: InspectionResource[];
  pods: InspectionPod[];
  networkPolicies: InspectionNetworkPolicy[];
  events: InspectionEvent[];
  securityControls: InspectionSecurity[];
}

export interface EndpointMetricPoint {
  timestamp: string;
  queueDepth: number;
  runningRequests: number;
  replicas: number;
}

export interface EndpointMetrics {
  observedAt: string;
  windowSeconds: number;
  stepSeconds: number;
  current: {
    queueDepth: number;
    runningRequests: number;
    replicas: number;
    requestsTotal: number;
  };
  series: EndpointMetricPoint[];
}

export interface InferenceResult {
  text: string;
  ttftMs: number;
  totalMs: number;
}

export interface PerformanceSample {
  id: string;
  endpointID: string;
  kind: "warm" | "activation";
  cacheState: string;
  ttftMs: number;
  totalMs: number;
  recordedAt: string;
}

export interface LoadRun {
  startedAt: string;
  requests: number;
  succeeded: number;
  failed: number;
  p50TTFTMs: number;
  p95TTFTMs: number;
  p50TotalMs: number;
  throughputRPS: number;
  durationMs: number;
}

export interface PhaseObservation {
  phase: EndpointPhase;
  reason: string;
  observedAt: string;
}
