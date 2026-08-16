import { useState } from "react";

import type { AuditEvent, Endpoint } from "../types";
import { formatRelativeTime, shortID } from "../utils";
import { Icon } from "./Icon";
import { Button, Panel, Tag } from "./Primitives";

interface EvidencePanelProps {
  endpoint: Endpoint;
  events: AuditEvent[];
  logs: string;
  error: string;
  onRefresh: () => void;
}

export function EvidencePanel({ endpoint, events, logs, error, onRefresh }: EvidencePanelProps) {
  const [tab, setTab] = useState<"audit" | "logs">("audit");
  return (
    <Panel
      action={
        <div className="evidence-actions">
          <div className="segmented-control">
            <button className={tab === "audit" ? "active" : ""} onClick={() => setTab("audit")}>
              <Icon name="database" size={14} /> Audit
            </button>
            <button className={tab === "logs" ? "active" : ""} onClick={() => setTab("logs")}>
              <Icon name="terminal" size={14} /> Engine logs
            </button>
          </div>
          <Button icon="refresh" variant="ghost" onClick={onRefresh}>Refresh</Button>
        </div>
      }
      className="evidence-panel"
      eyebrow="Retained and bounded"
      icon="terminal"
      id="evidence"
      title="Operational evidence"
    >
      {tab === "audit" ? (
        <div className="audit-layout">
          <div className="audit-timeline">
            {events.length > 0 ? events.map((event) => (
              <article key={event.id}>
                <span className={`audit-marker result-${event.result.startsWith("http_2") || event.result === "succeeded" || event.result === "accepted" ? "ok" : "other"}`}>
                  <Icon name={auditIcon(event.action)} size={13} />
                </span>
                <div>
                  <div className="audit-action"><strong>{event.action}</strong><Tag tone={resultTone(event.result)}>{event.result}</Tag></div>
                  <p>{auditSummary(event)}</p>
                  <small>request <code>{shortID(event.requestID, 16)}</code> · {formatRelativeTime(event.createdAt)}</small>
                </div>
              </article>
            )) : <div className="quiet-empty">No audit events have been recorded for this endpoint.</div>}
          </div>
          <aside className="audit-contract">
            <span className="stat-icon stat-icon-violet"><Icon name="database" /></span>
            <h3>Append-only history</h3>
            <p>Audit rows outlive the custom resource and a database trigger rejects updates or deletes.</p>
            <dl>
              <div><dt>Endpoint UID</dt><dd><code>{shortID(endpoint.runtime?.uid, 18)}</code></dd></div>
              <div><dt>Events retained</dt><dd>{events.length}</dd></div>
              <div><dt>Runtime truth</dt><dd>Kubernetes CR</dd></div>
              <div><dt>Presentation truth</dt><dd>Postgres</dd></div>
            </dl>
          </aside>
        </div>
      ) : (
        <div className="log-console">
          <div className="console-bar">
            <span><i className="console-dot red" /><i className="console-dot amber" /><i className="console-dot green" /></span>
            <code>{endpoint.runtime?.placement?.node ?? "engine pod"} · last 250 lines · max 256 KiB</code>
            <Tag tone="safe"><Icon name="shield" size={12} /> redacted</Tag>
          </div>
          <pre>{logs || error || "Engine logs are not available until a serving Pod exists."}</pre>
        </div>
      )}
      {error && tab === "audit" && <div className="evidence-error">{error}</div>}
    </Panel>
  );
}

function auditIcon(action: string): Parameters<typeof Icon>[0]["name"] {
  if (action.includes("create")) return "spark";
  if (action.includes("delete")) return "trash";
  if (action.includes("inference")) return "bot";
  if (action.includes("logs")) return "terminal";
  if (action.includes("stream")) return "activity";
  return "database";
}

function resultTone(result: string): string {
  if (result === "succeeded" || result === "accepted" || result === "replayed" || result.startsWith("http_2")) return "safe";
  if (result === "failed" || result.startsWith("http_5")) return "warning";
  return "neutral";
}

function auditSummary(event: AuditEvent): string {
  const details = event.details ?? {};
  if (event.action === "endpoint.create") {
    return details.idempotencyReplay ? "The original endpoint reservation was safely replayed." : "Gateway confirmed the Kubernetes custom resource.";
  }
  if (event.action.includes("inference")) {
    return "An owner-scoped completion crossed the Control API and Gateway boundary.";
  }
  if (event.action.includes("delete")) {
    return "Finalizer-driven Kubernetes reclamation was requested or confirmed.";
  }
  return "Correlated product and control-plane operation.";
}
