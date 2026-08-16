import type { Endpoint, PhaseObservation } from "../types";
import { currentReason, formatDuration, formatRelativeTime } from "../utils";
import { Icon } from "./Icon";
import { Panel, Tag } from "./Primitives";

interface LifecyclePanelProps {
  endpoint: Endpoint;
  observations: PhaseObservation[];
}

export function LifecyclePanel({ endpoint, observations }: LifecyclePanelProps) {
  const runtime = endpoint.runtime;
  const ready = (runtime?.replicas?.ready ?? 0) > 0;
  const cacheResolved = Boolean(runtime?.placement?.cacheState);
  const scheduled = Boolean(runtime?.placement?.node);
  const deleted = runtime?.phase === "Deleted";
  const scaled = (runtime?.replicas?.desired ?? 0) > 1;
  const steps = [
    {
      label: "Intent accepted",
      detail: "CR reserved",
      state: "done",
      icon: "spark" as const
    },
    {
      label: "Weights resolved",
      detail: runtime?.placement?.cacheState || "cache pending",
      state: cacheResolved ? "done" : "active",
      icon: "database" as const
    },
    {
      label: "GPU scheduled",
      detail: runtime?.placement?.node || "awaiting placement",
      state: scheduled ? "done" : cacheResolved ? "active" : "pending",
      icon: "gpu" as const
    },
    {
      label: "Engine serving",
      detail: ready ? `${runtime?.replicas?.ready} ready` : currentReason(endpoint),
      state: ready || deleted ? "done" : scheduled ? "active" : "pending",
      icon: "bot" as const
    },
    {
      label: "Demand managed",
      detail: scaled ? `${runtime?.replicas?.desired} replicas` : "KEDA watching",
      state: ready || runtime?.phase === "Ready" ? "done" : "pending",
      icon: "chart" as const
    },
    {
      label: "Reclaimed",
      detail: deleted ? "metadata retained" : "finalizer armed",
      state: deleted ? "done" : runtime?.phase === "Deleting" ? "active" : "pending",
      icon: "trash" as const
    }
  ];

  return (
    <Panel
      action={<Tag tone="live"><span className="live-dot" /> CR status stream</Tag>}
      className="lifecycle-panel"
      eyebrow="Reconcile evidence"
      icon="activity"
      title="Endpoint lifecycle"
    >
      <div className="lifecycle-lane">
        {steps.map((step, index) => (
          <div className={`lifecycle-step state-${step.state}`} key={step.label}>
            <div className="lifecycle-icon"><Icon name={step.icon} size={17} /></div>
            <div>
              <strong>{step.label}</strong>
              <small>{step.detail}</small>
            </div>
            {index < steps.length - 1 && <span className="lifecycle-connector" />}
          </div>
        ))}
      </div>

      <div className="lifecycle-detail-grid">
        <div className="observation-timeline">
          <div className="subsection-heading">
            <div>
              <span className="eyebrow">Browser-observed transitions</span>
              <h3>Live phase trace</h3>
            </div>
            <small>session evidence, not synthetic history</small>
          </div>
          <div className="observation-list">
            {observations.length > 0 ? (
              observations.slice(-8).reverse().map((observation, index) => {
                const next = observations[observations.length - index];
                const duration = next
                  ? new Date(next.observedAt).getTime() - new Date(observation.observedAt).getTime()
                  : Date.now() - new Date(observation.observedAt).getTime();
                return (
                  <article key={`${observation.observedAt}-${observation.phase}`}>
                    <span className="observation-marker" />
                    <div>
                      <strong>{observation.phase}</strong>
                      <p>{observation.reason}</p>
                    </div>
                    <span>
                      {formatRelativeTime(observation.observedAt)}
                      <small>{formatDuration(duration)}</small>
                    </span>
                  </article>
                );
              })
            ) : (
              <div className="quiet-empty">Waiting for the first status event.</div>
            )}
          </div>
        </div>

        <div className="condition-stack">
          <div className="subsection-heading">
            <div>
              <span className="eyebrow">Authoritative CR conditions</span>
              <h3>Reason codes</h3>
            </div>
          </div>
          {(runtime?.conditions ?? []).length > 0 ? (
            <div className="condition-list">
              {(runtime?.conditions ?? []).map((condition) => (
                <article className={`condition-card condition-${condition.status.toLowerCase()}`} key={condition.type}>
                  <div className="condition-card-top">
                    <span className="condition-icon">
                      <Icon name={condition.status === "True" ? "check" : "clock"} size={14} />
                    </span>
                    <strong>{condition.type}</strong>
                    <Tag tone={condition.status === "True" ? "safe" : "neutral"}>{condition.status}</Tag>
                  </div>
                  <code>{condition.reason}</code>
                  <p>{condition.message}</p>
                  <small>transitioned {formatRelativeTime(condition.lastTransitionTime)}</small>
                </article>
              ))}
            </div>
          ) : (
            <div className="quiet-empty">Conditions will appear after the operator observes the endpoint.</div>
          )}
        </div>
      </div>
    </Panel>
  );
}
