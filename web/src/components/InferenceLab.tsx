import { useMemo, useRef, useState } from "react";

import { APIError, streamCompletion } from "../api";
import type { Endpoint, EndpointMetrics, LoadRun, PerformanceSample } from "../types";
import { classifyPerformanceSample, formatDuration, percentile } from "../utils";
import { Icon } from "./Icon";
import { Button, InlineError, Panel, Tag } from "./Primitives";

interface InferenceLabProps {
  endpoint: Endpoint;
  metrics: EndpointMetrics | null;
  onEvidenceChanged: () => void;
  onSample: (sample: PerformanceSample) => void;
}

interface ChatMessage {
  id: string;
  role: "user" | "assistant";
  text: string;
  meta?: string;
  pending?: boolean;
}

export function InferenceLab({ endpoint, metrics, onEvidenceChanged, onSample }: InferenceLabProps) {
  const [mode, setMode] = useState<"chat" | "load">("chat");
  const [messages, setMessages] = useState<ChatMessage[]>([
    {
      id: "welcome",
      role: "assistant",
      text: "I am the repository-owned mock engine behind this endpoint. Ask what Ember reconciled, or use Load Lab to drive queue-depth autoscaling.",
      meta: "simulation disclosure"
    }
  ]);
  const [prompt, setPrompt] = useState("Explain what Kubernetes resources Ember created for this endpoint.");
  const [sending, setSending] = useState(false);
  const [activation, setActivation] = useState("");
  const [error, setError] = useState("");
  const abortRef = useRef<AbortController | null>(null);

  const [concurrency, setConcurrency] = useState(4);
  const [requestCount, setRequestCount] = useState(12);
  const [loadPrompt, setLoadPrompt] = useState("Summarize Ember's autoscaling control loop.");
  const [loadRunning, setLoadRunning] = useState(false);
  const [loadProgress, setLoadProgress] = useState(0);
  const [loadRun, setLoadRun] = useState<LoadRun | null>(null);

  const canInfer = !endpoint.deletedAt && endpoint.runtime?.phase !== "Deleting";
  const queuePressure = useMemo(() => {
    const queue = metrics?.current.queueDepth ?? 0;
    const target = endpoint.targetQueueDepth || 1;
    return Math.min(100, (queue / target) * 100);
  }, [endpoint.targetQueueDepth, metrics?.current.queueDepth]);

  const send = async () => {
    const cleanPrompt = prompt.trim();
    if (!cleanPrompt || sending || !canInfer) {
      return;
    }
    setError("");
    setActivation("");
    setSending(true);
    const userID = crypto.randomUUID();
    const assistantID = crypto.randomUUID();
    setMessages((current) => [
      ...current,
      { id: userID, role: "user", text: cleanPrompt },
      { id: assistantID, role: "assistant", text: "", pending: true }
    ]);
    setPrompt("");
    const controller = new AbortController();
    abortRef.current = controller;
    let activated = false;
    try {
      const result = await completionWithActivation(
        endpoint,
        cleanPrompt,
        (text) => {
          setMessages((current) =>
            current.map((message) =>
              message.id === assistantID ? { ...message, text, pending: true } : message
            )
          );
        },
        (message) => {
          activated = true;
          setActivation(message);
        },
        controller.signal
      );
      setMessages((current) =>
        current.map((message) =>
          message.id === assistantID
            ? {
                ...message,
                text: result.text || "The engine returned an empty completion.",
                pending: false,
                meta: `TTFT ${formatDuration(result.ttftMs)} · total ${formatDuration(result.totalMs)}`
              }
            : message
        )
      );
      onSample(sampleFrom(endpoint, result.ttftMs, result.totalMs, activated));
      onEvidenceChanged();
      setActivation("");
    } catch (sendError) {
      if (controller.signal.aborted) {
        setMessages((current) => current.filter((message) => message.id !== assistantID));
      } else {
        const message = sendError instanceof Error ? sendError.message : "Inference failed";
        setError(message);
        setMessages((current) =>
          current.map((item) =>
            item.id === assistantID ? { ...item, text: `Request failed: ${message}`, pending: false } : item
          )
        );
      }
    } finally {
      abortRef.current = null;
      setSending(false);
    }
  };

  const runLoad = async () => {
    if (loadRunning || !canInfer) {
      return;
    }
    setError("");
    setLoadRunning(true);
    setLoadProgress(0);
    const startedAt = performance.now();
    let completed = 0;
    const samples: PerformanceSample[] = [];
    const failures: Error[] = [];
    const queue = Array.from({ length: requestCount }, (_, index) => index);
    const workers = Array.from({ length: Math.min(concurrency, requestCount) }, async () => {
      while (queue.length > 0) {
        const index = queue.shift();
        if (index === undefined) {
          return;
        }
        try {
          const { result, activated } = await loadCompletionWithActivation(endpoint, `${loadPrompt} [request ${index + 1}]`);
          const sample = sampleFrom(endpoint, result.ttftMs, result.totalMs, activated);
          samples.push(sample);
          onSample(sample);
        } catch (loadError) {
          failures.push(loadError instanceof Error ? loadError : new Error("request failed"));
        } finally {
          completed += 1;
          setLoadProgress(completed);
        }
      }
    });
    await Promise.all(workers);
    const durationMs = performance.now() - startedAt;
    const ttft = samples.map((sample) => sample.ttftMs);
    const total = samples.map((sample) => sample.totalMs);
    setLoadRun({
      startedAt: new Date().toISOString(),
      requests: requestCount,
      succeeded: samples.length,
      failed: failures.length,
      p50TTFTMs: percentile(ttft, 50),
      p95TTFTMs: percentile(ttft, 95),
      p50TotalMs: percentile(total, 50),
      throughputRPS: durationMs > 0 ? (samples.length / durationMs) * 1000 : 0,
      durationMs
    });
    if (failures.length > 0) {
      setError(`${failures.length} request${failures.length === 1 ? "" : "s"} failed; the successful samples are retained.`);
    }
    setLoadRunning(false);
    onEvidenceChanged();
  };

  return (
    <Panel
      action={
        <div className="segmented-control">
          <button className={mode === "chat" ? "active" : ""} onClick={() => setMode("chat")}>
            <Icon name="bot" size={14} /> Chat
          </button>
          <button className={mode === "load" ? "active" : ""} onClick={() => setMode("load")}>
            <Icon name="activity" size={14} /> Load Lab
          </button>
        </div>
      }
      className="inference-panel"
      eyebrow="OpenAI-compatible path"
      icon="bot"
      id="inference"
      title="Inference laboratory"
    >
      <div className="inference-status-strip">
        <span><span className="live-dot" /> /v1/chat/completions</span>
        <span>model <code>{endpoint.modelID}</code></span>
        <Tag tone={endpoint.runtime?.replicas?.ready ? "safe" : "warning"}>
          {endpoint.runtime?.replicas?.ready ?? 0} ready
        </Tag>
      </div>

      {mode === "chat" ? (
        <div className="chat-layout">
          <div className="chat-window">
            <div className="chat-messages">
              {messages.map((message) => (
                <article className={`chat-message ${message.role}`} key={message.id}>
                  <span className="message-avatar">
                    <Icon name={message.role === "assistant" ? "flame" : "bot"} size={15} />
                  </span>
                  <div>
                    <div className="message-label">
                      <strong>{message.role === "assistant" ? "Ember endpoint" : "You"}</strong>
                      {message.meta && <small>{message.meta}</small>}
                    </div>
                    <p>{message.text || (message.pending ? "Waiting for first token…" : "")}</p>
                    {message.pending && <span className="typing-dots"><i /><i /><i /></span>}
                  </div>
                </article>
              ))}
            </div>
            {activation && (
              <div className="activation-banner">
                <span className="activation-pulse"><Icon name="spark" size={15} /></span>
                <div><strong>Scale-from-zero in progress</strong><p>{activation}</p></div>
              </div>
            )}
            <div className="chat-composer">
              <textarea
                disabled={sending || !canInfer}
                onChange={(event) => setPrompt(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === "Enter" && !event.shiftKey) {
                    event.preventDefault();
                    void send();
                  }
                }}
                placeholder="Ask the endpoint…"
                rows={3}
                value={prompt}
              />
              <button
                aria-label={sending ? "Stop generation" : "Send message"}
                className="send-button"
                disabled={!canInfer || (!sending && !prompt.trim())}
                onClick={() => {
                  if (sending) {
                    abortRef.current?.abort();
                  } else {
                    void send();
                  }
                }}
              >
                <Icon name={sending ? "close" : "send"} size={17} />
              </button>
            </div>
          </div>
          <aside className="chat-evidence">
            <div>
              <span className="eyebrow">Request path</span>
              <h3>What this proves</h3>
            </div>
            {[
              ["Browser", "opaque HttpOnly session", "shield"],
              ["Control API", "short-lived owner JWT", "code"],
              ["Gateway", "owner check + activation", "network"],
              ["Engine", "credentials stripped", "gpu"]
            ].map(([title, detail, icon], index) => (
              <div className="request-hop" key={title}>
                <span><Icon name={icon as Parameters<typeof Icon>[0]["name"]} size={15} /></span>
                <div><strong>{title}</strong><small>{detail}</small></div>
                {index < 3 && <i />}
              </div>
            ))}
            <p>The browser never receives Kubernetes credentials, the Gateway private key or an engine bearer token.</p>
          </aside>
        </div>
      ) : (
        <div className="load-lab">
          <div className="load-controls">
            <div className="load-control-heading">
              <div>
                <span className="eyebrow">Controlled pressure</span>
                <h3>Drive queue-depth autoscaling</h3>
              </div>
              <Tag tone="warning">browser generated</Tag>
            </div>
            <label className="field">
              <span>Prompt</span>
              <textarea rows={3} value={loadPrompt} onChange={(event) => setLoadPrompt(event.target.value)} />
            </label>
            <div className="range-field">
              <div><span>Concurrency</span><strong>{concurrency}</strong></div>
              <input max={12} min={1} type="range" value={concurrency} onChange={(event) => setConcurrency(Number(event.target.value))} />
            </div>
            <div className="range-field">
              <div><span>Total requests</span><strong>{requestCount}</strong></div>
              <input max={24} min={2} type="range" value={requestCount} onChange={(event) => setRequestCount(Number(event.target.value))} />
            </div>
            <Button disabled={loadRunning || !canInfer} icon="activity" variant="primary" onClick={() => void runLoad()}>
              {loadRunning ? `Running ${loadProgress}/${requestCount}` : "Start load run"}
            </Button>
            {loadRunning && (
              <div className="load-progress">
                <span style={{ width: `${(loadProgress / requestCount) * 100}%` }} />
              </div>
            )}
          </div>

          <div className="load-results">
            <div className="pressure-gauge">
              <div>
                <span>Live queue pressure</span>
                <strong>{Math.round(metrics?.current.queueDepth ?? 0)}</strong>
                <small>target {endpoint.targetQueueDepth}</small>
              </div>
              <div className="pressure-track"><span style={{ height: `${queuePressure}%` }} /></div>
              <div className="replica-stack">
                {Array.from({ length: endpoint.maxReplicas }, (_, index) => (
                  <span
                    className={index < (endpoint.runtime?.replicas?.desired ?? 0) ? "replica active" : "replica"}
                    key={index}
                  >
                    <Icon name="gpu" size={13} />
                  </span>
                ))}
              </div>
            </div>
            {loadRun ? (
              <div className="load-result-grid">
                <ResultStat label="Success" value={`${loadRun.succeeded}/${loadRun.requests}`} />
                <ResultStat label="p50 TTFT" value={formatDuration(loadRun.p50TTFTMs)} />
                <ResultStat label="p95 TTFT" value={formatDuration(loadRun.p95TTFTMs)} />
                <ResultStat label="Throughput" value={`${loadRun.throughputRPS.toFixed(2)} rps`} />
                <ResultStat label="Run time" value={formatDuration(loadRun.durationMs)} />
                <ResultStat label="Failures" value={loadRun.failed.toString()} />
              </div>
            ) : (
              <div className="load-placeholder">
                <Icon name="chart" size={28} />
                <h3>Observe 1 → N → 1</h3>
                <p>Run a bounded burst, then watch Prometheus queue depth and the KEDA-managed replica line move together above.</p>
              </div>
            )}
          </div>
        </div>
      )}
      {error && <InlineError message={error} />}
    </Panel>
  );
}

async function completionWithActivation(
  endpoint: Endpoint,
  prompt: string,
  onDelta: (text: string) => void,
  onActivation: (message: string) => void,
  signal: AbortSignal
) {
  for (let attempt = 0; attempt < 24; attempt += 1) {
    try {
      return await streamCompletion(endpoint.id, endpoint.modelID, prompt, onDelta, signal);
    } catch (error) {
      if (!(error instanceof APIError) || error.code !== "endpoint_activating") {
        throw error;
      }
      const delay = Math.max(1, error.retryAfterSeconds || 5);
      onActivation(`Gateway accepted activity; retrying in ${delay}s while the operator restores a replica.`);
      await sleep(delay * 1000, signal);
    }
  }
  throw new Error("Endpoint activation did not complete within the retry window");
}

async function loadCompletionWithActivation(endpoint: Endpoint, prompt: string) {
  let activated = false;
  for (let attempt = 0; attempt < 12; attempt += 1) {
    try {
      const result = await streamCompletion(endpoint.id, endpoint.modelID, prompt);
      return { result, activated };
    } catch (error) {
      if (!(error instanceof APIError) || error.code !== "endpoint_activating") {
        throw error;
      }
      activated = true;
      await sleep(Math.max(1, error.retryAfterSeconds || 5) * 1000);
    }
  }
  throw new Error("Endpoint activation timed out");
}

function sampleFrom(endpoint: Endpoint, ttftMs: number, totalMs: number, activated: boolean): PerformanceSample {
  return {
    id: crypto.randomUUID(),
    endpointID: endpoint.id,
    kind: classifyPerformanceSample(activated),
    cacheState: endpoint.runtime?.placement?.cacheState ?? "unknown",
    ttftMs,
    totalMs,
    recordedAt: new Date().toISOString()
  };
}

function sleep(milliseconds: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = window.setTimeout(resolve, milliseconds);
    signal?.addEventListener(
      "abort",
      () => {
        window.clearTimeout(timer);
        reject(new DOMException("Request aborted", "AbortError"));
      },
      { once: true }
    );
  });
}

function ResultStat({ label, value }: { label: string; value: string }) {
  return <div><small>{label}</small><strong>{value}</strong></div>;
}
