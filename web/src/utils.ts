import type {
  Endpoint,
  EndpointMetricPoint,
  EndpointPhase,
  PerformanceSample,
  PhaseObservation
} from "./types";

export function formatBytes(value?: number): string {
  if (!value || value <= 0) {
    return "—";
  }
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  const order = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1);
  return `${(value / 1024 ** order).toFixed(order >= 3 ? 1 : 0)} ${units[order]}`;
}

export function formatDuration(milliseconds: number): string {
  if (!Number.isFinite(milliseconds) || milliseconds < 0) {
    return "—";
  }
  if (milliseconds < 1000) {
    return `${Math.round(milliseconds)} ms`;
  }
  if (milliseconds < 60_000) {
    return `${(milliseconds / 1000).toFixed(milliseconds < 10_000 ? 1 : 0)} s`;
  }
  const minutes = Math.floor(milliseconds / 60_000);
  const seconds = Math.round((milliseconds % 60_000) / 1000);
  return `${minutes}m ${seconds}s`;
}

export function formatRelativeTime(raw?: string, now = Date.now()): string {
  if (!raw) {
    return "never";
  }
  const elapsed = Math.max(0, now - new Date(raw).getTime());
  if (elapsed < 10_000) {
    return "just now";
  }
  if (elapsed < 60_000) {
    return `${Math.floor(elapsed / 1000)}s ago`;
  }
  if (elapsed < 3_600_000) {
    return `${Math.floor(elapsed / 60_000)}m ago`;
  }
  if (elapsed < 86_400_000) {
    return `${Math.floor(elapsed / 3_600_000)}h ago`;
  }
  return `${Math.floor(elapsed / 86_400_000)}d ago`;
}

export function phaseTone(phase?: EndpointPhase): string {
  switch (phase) {
    case "Ready":
      return "positive";
    case "Progressing":
    case "Creating":
    case "Pending":
      return "active";
    case "Degraded":
      return "negative";
    case "Deleting":
      return "warning";
    case "Deleted":
      return "muted";
    default:
      return "muted";
  }
}

export function currentReason(endpoint: Endpoint): string {
  const conditions = endpoint.runtime?.conditions ?? [];
  const active =
    conditions.find((condition) => condition.type === "Degraded" && condition.status === "True") ??
    conditions.find((condition) => condition.type === "Progressing" && condition.status === "True") ??
    conditions.find((condition) => condition.type === "Ready" && condition.status === "True");
  return active?.reason ?? endpoint.runtime?.phase ?? "Runtime unavailable";
}

export function stableIdempotencyKey(): string {
  const cryptoAPI = globalThis.crypto;
  if (cryptoAPI?.randomUUID) {
    return `ui-create-${cryptoAPI.randomUUID()}`;
  }
  if (cryptoAPI?.getRandomValues) {
    const bytes = new Uint8Array(16);
    cryptoAPI.getRandomValues(bytes);
    return `ui-create-${Array.from(bytes, (value) => value.toString(16).padStart(2, "0")).join("")}`;
  }
  return `ui-create-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

export function percentile(values: number[], percentileValue: number): number {
  if (values.length === 0) {
    return 0;
  }
  const sorted = [...values].sort((left, right) => left - right);
  const index = Math.min(sorted.length - 1, Math.max(0, Math.ceil((percentileValue / 100) * sorted.length) - 1));
  return sorted[index];
}

export function recordPhaseObservation(
  history: PhaseObservation[],
  phase: EndpointPhase,
  reason: string,
  observedAt = new Date().toISOString()
): PhaseObservation[] {
  const previous = history[history.length - 1];
  if (previous?.phase === phase && previous.reason === reason) {
    return history;
  }
  return [...history, { phase, reason, observedAt }].slice(-40);
}

export function classifyPerformanceSample(activated: boolean): PerformanceSample["kind"] {
  return activated ? "activation" : "warm";
}

export function chartPolyline(
  points: EndpointMetricPoint[],
  key: "queueDepth" | "runningRequests" | "replicas",
  width: number,
  height: number,
  maximum?: number
): string {
  if (points.length === 0) {
    return "";
  }
  const max = Math.max(maximum ?? 0, ...points.map((point) => point[key]), 1);
  return points
    .map((point, index) => {
      const x = points.length === 1 ? width : (index / (points.length - 1)) * width;
      const y = height - (point[key] / max) * height;
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
}

export function mergeEndpointRuntime(endpoint: Endpoint, raw: unknown): Endpoint {
  if (!raw || typeof raw !== "object") {
    return endpoint;
  }
  const resource = raw as {
    metadata?: { uid?: string };
    status?: Endpoint["runtime"];
  };
  if (!resource.status) {
    return endpoint;
  }
  return {
    ...endpoint,
    runtime: {
      ...resource.status,
      uid: resource.metadata?.uid ?? endpoint.runtime?.uid
    }
  };
}

export function shortID(value?: string, length = 10): string {
  if (!value) {
    return "—";
  }
  return value.length <= length ? value : `${value.slice(0, length)}…`;
}
