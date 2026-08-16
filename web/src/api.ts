import type {
  AuditEvent,
  Catalog,
  CreateEndpointInput,
  Endpoint,
  EndpointInspection,
  EndpointMetrics,
  InferenceResult,
  Session
} from "./types";

export class APIError extends Error {
  readonly status: number;
  readonly code: string;
  readonly retryAfterSeconds: number;

  constructor(status: number, code: string, message: string, retryAfterSeconds = 0) {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.code = code;
    this.retryAfterSeconds = retryAfterSeconds;
  }
}

interface APIErrorPayload {
  error?: {
    code?: string;
    message?: string;
  };
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const response = await fetch(path, {
    ...init,
    headers,
    credentials: "same-origin"
  });
  if (!response.ok) {
    throw await responseError(response);
  }
  return (await response.json()) as T;
}

async function responseError(response: Response): Promise<APIError> {
  let payload: APIErrorPayload = {};
  try {
    payload = (await response.json()) as APIErrorPayload;
  } catch {
    payload = {};
  }
  const retryAfterSeconds = Number.parseInt(response.headers.get("Retry-After") ?? "0", 10) || 0;
  return new APIError(
    response.status,
    payload.error?.code ?? "request_failed",
    payload.error?.message ?? `Request failed with ${response.status}`,
    retryAfterSeconds
  );
}

export function bootstrapSession(): Promise<Session> {
  return request<Session>("/api/v1/session", { method: "POST" });
}

export function getCatalog(): Promise<Catalog> {
  return request<Catalog>("/api/v1/catalog/models");
}

export async function listEndpoints(): Promise<Endpoint[]> {
  const payload = await request<{ endpoints: Endpoint[] }>("/api/v1/endpoints");
  return payload.endpoints;
}

export function getEndpoint(endpointID: string): Promise<Endpoint> {
  return request<Endpoint>(`/api/v1/endpoints/${encodeURIComponent(endpointID)}`);
}

export function createEndpoint(input: CreateEndpointInput, idempotencyKey: string): Promise<Endpoint> {
  return request<Endpoint>("/api/v1/endpoints", {
    method: "POST",
    headers: { "Idempotency-Key": idempotencyKey },
    body: JSON.stringify(input)
  });
}

export function deleteEndpoint(endpointID: string): Promise<Endpoint> {
  return request<Endpoint>(`/api/v1/endpoints/${encodeURIComponent(endpointID)}`, {
    method: "DELETE"
  });
}

export async function getAuditEvents(endpointID: string): Promise<AuditEvent[]> {
  const payload = await request<{ events: AuditEvent[] }>(
    `/api/v1/endpoints/${encodeURIComponent(endpointID)}/events?limit=200`
  );
  return payload.events;
}

export async function getLogs(endpointID: string, tail = 250): Promise<string> {
  const response = await fetch(
    `/api/v1/endpoints/${encodeURIComponent(endpointID)}/logs?tail=${tail}`,
    { credentials: "same-origin" }
  );
  if (!response.ok) {
    throw await responseError(response);
  }
  return response.text();
}

export function getInspection(endpointID: string): Promise<EndpointInspection> {
  return request<EndpointInspection>(`/api/v1/endpoints/${encodeURIComponent(endpointID)}/inspect`);
}

export function getMetrics(endpointID: string): Promise<EndpointMetrics> {
  return request<EndpointMetrics>(
    `/api/v1/endpoints/${encodeURIComponent(endpointID)}/metrics?window=900&step=5`
  );
}

export async function streamCompletion(
  endpointID: string,
  modelID: string,
  prompt: string,
  onDelta?: (text: string) => void,
  signal?: AbortSignal
): Promise<InferenceResult> {
  const startedAt = performance.now();
  const response = await fetch(
    `/api/v1/endpoints/${encodeURIComponent(endpointID)}/v1/chat/completions`,
    {
      method: "POST",
      credentials: "same-origin",
      headers: {
        Accept: "text/event-stream",
        "Content-Type": "application/json"
      },
      body: JSON.stringify({
        model: modelID,
        stream: true,
        messages: [{ role: "user", content: prompt }]
      }),
      signal
    }
  );
  if (!response.ok) {
    throw await responseError(response);
  }
  if (!response.body) {
    throw new APIError(502, "stream_unavailable", "Inference response did not contain a stream");
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let text = "";
  let firstTokenAt = 0;
  for (;;) {
    const { value, done } = await reader.read();
    buffer += decoder.decode(value, { stream: !done });
    const frames = buffer.split(/\r?\n\r?\n/);
    buffer = frames.pop() ?? "";
    for (const frame of frames) {
      for (const line of frame.split(/\r?\n/)) {
        if (!line.startsWith("data:")) {
          continue;
        }
        const data = line.slice(5).trim();
        if (!data || data === "[DONE]") {
          continue;
        }
        const chunk = JSON.parse(data) as {
          choices?: Array<{ delta?: { content?: string } }>;
        };
        const delta = chunk.choices?.[0]?.delta?.content ?? "";
        if (!delta) {
          continue;
        }
        if (!firstTokenAt) {
          firstTokenAt = performance.now();
        }
        text += delta;
        onDelta?.(text);
      }
    }
    if (done) {
      break;
    }
  }
  const completedAt = performance.now();
  return {
    text,
    ttftMs: (firstTokenAt || completedAt) - startedAt,
    totalMs: completedAt - startedAt
  };
}
