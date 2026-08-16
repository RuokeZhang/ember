import { useCallback, useEffect, useMemo, useState } from "react";

import { getAuditEvents, getInspection, getLogs, getMetrics } from "../api";
import { usePhaseHistory, useStoredState } from "../hooks";
import type {
  AuditEvent,
  Catalog,
  Endpoint,
  EndpointInspection,
  EndpointMetrics,
  PerformanceSample
} from "../types";
import {
  currentReason,
  formatBytes,
  formatRelativeTime,
  shortID
} from "../utils";
import { DeleteDialog } from "./DeleteDialog";
import { EvidencePanel } from "./EvidencePanel";
import { Icon } from "./Icon";
import { InferenceLab } from "./InferenceLab";
import { InspectorPanel } from "./InspectorPanel";
import { LifecyclePanel } from "./LifecyclePanel";
import { MetricsPanel } from "./MetricsPanel";
import { Button, PhaseBadge, Tag } from "./Primitives";

interface EndpointDashboardProps {
  endpoint: Endpoint;
  catalog: Catalog;
  onRefresh: () => void;
  onDelete: (endpointID: string) => Promise<void>;
}

export function EndpointDashboard({ endpoint, catalog, onRefresh, onDelete }: EndpointDashboardProps) {
  const [metrics, setMetrics] = useState<EndpointMetrics | null>(null);
  const [inspection, setInspection] = useState<EndpointInspection | null>(null);
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [logs, setLogs] = useState("");
  const [metricsError, setMetricsError] = useState("");
  const [inspectionError, setInspectionError] = useState("");
  const [evidenceError, setEvidenceError] = useState("");
  const [deleteOpen, setDeleteOpen] = useState(false);
  const observations = usePhaseHistory(endpoint);
  const [samples, setSamples] = useStoredState<PerformanceSample[]>(
    `ember.performance-samples.${endpoint.id}`,
    []
  );

  const model = useMemo(
    () => catalog.models.find((candidate) => candidate.id === endpoint.modelID),
    [catalog.models, endpoint.modelID]
  );
  const profile = useMemo(
    () => catalog.profiles.find((candidate) => candidate.name === endpoint.profile),
    [catalog.profiles, endpoint.profile]
  );
  const isDeleted = endpoint.runtime?.phase === "Deleted" || Boolean(endpoint.deletedAt);
  const isDeleting = endpoint.runtime?.phase === "Deleting" || Boolean(endpoint.deletionRequestedAt && !endpoint.deletedAt);

  const refreshTelemetry = useCallback(async () => {
    if (isDeleted || !endpoint.runtime?.uid) {
      return;
    }
    const [metricsResult, inspectionResult] = await Promise.allSettled([
      getMetrics(endpoint.id),
      getInspection(endpoint.id)
    ]);
    if (metricsResult.status === "fulfilled") {
      setMetrics(metricsResult.value);
      setMetricsError("");
    } else {
      setMetricsError("Prometheus series are not available yet");
    }
    if (inspectionResult.status === "fulfilled") {
      setInspection(inspectionResult.value);
      setInspectionError("");
    } else {
      setInspectionError("Workload resources are still converging");
    }
  }, [endpoint.id, endpoint.runtime?.uid, isDeleted]);

  const refreshEvidence = useCallback(async () => {
    const eventPromise = getAuditEvents(endpoint.id);
    const logPromise =
      isDeleted || !endpoint.runtime?.uid
        ? Promise.resolve("")
        : getLogs(endpoint.id).catch((error: unknown) => {
            setEvidenceError(error instanceof Error ? error.message : "Logs are unavailable");
            return "";
          });
    const [nextEvents, nextLogs] = await Promise.all([eventPromise, logPromise]);
    setEvents(nextEvents);
    setLogs(nextLogs);
    if (nextLogs || isDeleted) {
      setEvidenceError("");
    }
  }, [endpoint.id, endpoint.runtime?.uid, isDeleted]);

  useEffect(() => {
    setMetrics(null);
    setInspection(null);
    setEvents([]);
    setLogs("");
    setMetricsError("");
    setInspectionError("");
    setEvidenceError("");
    void refreshTelemetry();
    void refreshEvidence().catch((error: unknown) => {
      setEvidenceError(error instanceof Error ? error.message : "Evidence is unavailable");
    });
  }, [endpoint.id, refreshEvidence, refreshTelemetry]);

  useEffect(() => {
    if (isDeleted) {
      return;
    }
    const telemetryTimer = window.setInterval(() => void refreshTelemetry(), 5000);
    const evidenceTimer = window.setInterval(
      () => void refreshEvidence().catch(() => undefined),
      15000
    );
    return () => {
      window.clearInterval(telemetryTimer);
      window.clearInterval(evidenceTimer);
    };
  }, [isDeleted, refreshEvidence, refreshTelemetry]);

  const addSample = (sample: PerformanceSample) => {
    setSamples((current) => [...current, sample].slice(-80));
  };

  const allocatedGPUs = (endpoint.runtime?.replicas?.desired ?? 0) * (profile?.gpuCount ?? 0);
  const activeCondition = (endpoint.runtime?.conditions ?? []).find(
    (condition) => condition.status === "True" && ["Degraded", "Progressing", "Ready"].includes(condition.type)
  );

  return (
    <div className="page endpoint-page">
      <section className="endpoint-hero">
        <div className="endpoint-hero-main">
          <div className="endpoint-title-row">
            <span className="endpoint-large-icon"><Icon name="gpu" size={25} /></span>
            <div>
              <div className="endpoint-title-meta">
                <PhaseBadge phase={endpoint.runtime?.phase} />
                <span>endpoint/{endpoint.id}</span>
              </div>
              <h1>{endpoint.displayName}</h1>
              <p>{endpoint.modelID} <span>·</span> revision <code>{endpoint.revision}</code></p>
            </div>
          </div>
          <div className="endpoint-hero-actions">
            {!isDeleted && !isDeleting && (
              <Button icon="bot" variant="primary" onClick={() => document.getElementById("inference")?.scrollIntoView({ behavior: "smooth" })}>
                Open inference lab
              </Button>
            )}
            {!isDeleted && (
              <Button icon="trash" variant="ghost" onClick={() => setDeleteOpen(true)}>
                Delete
              </Button>
            )}
          </div>
        </div>
        <div className="endpoint-reason-bar">
          <span className={`reason-symbol phase-${endpoint.runtime?.phase?.toLowerCase() ?? "creating"}`}>
            <Icon name={endpoint.runtime?.phase === "Degraded" ? "shield" : endpoint.runtime?.phase === "Ready" ? "check" : "activity"} size={15} />
          </span>
          <div>
            <strong>{activeCondition?.reason ?? currentReason(endpoint)}</strong>
            <p>{activeCondition?.message ?? endpoint.runtimeError?.message ?? "Waiting for authoritative runtime evidence."}</p>
          </div>
          <span className="reason-age">{formatRelativeTime(activeCondition?.lastTransitionTime ?? endpoint.createdAt)}</span>
        </div>
      </section>

      {(isDeleting || isDeleted) && (
        <section className={`deletion-banner ${isDeleted ? "complete" : ""}`}>
          <div className="deletion-banner-icon"><Icon name={isDeleted ? "check" : "trash"} /></div>
          <div>
            <span className="eyebrow">{isDeleted ? "Runtime reclaimed" : "Finalizer active"}</span>
            <h2>{isDeleted ? "Kubernetes resources are gone; evidence remains." : "Ember is reclaiming the endpoint safely."}</h2>
            <p>
              {isDeleted
                ? "The CR and workload namespace are absent. Model metadata, owner history and audit events remain in Postgres."
                : "The operator scales down, waits for Pod disappearance and deletes the dedicated namespace before confirming deletion."}
            </p>
          </div>
          {!isDeleted && <Tag tone="warning"><span className="spinner small" /> cleanup in progress</Tag>}
        </section>
      )}

      <section className="endpoint-stat-strip">
        <HeaderStat icon="layers" label="Replicas" value={`${endpoint.runtime?.replicas?.ready ?? 0} / ${endpoint.runtime?.replicas?.desired ?? 0}`} detail={`max ${endpoint.maxReplicas}`} />
        <HeaderStat icon="gpu" label="GPU allocation" value={`${allocatedGPUs} L4`} detail={`${profile?.gpuCount ?? 0} per replica`} />
        <HeaderStat icon="database" label="Cache state" value={endpoint.runtime?.placement?.cacheState ?? "Pending"} detail={endpoint.runtime?.placement?.node ?? "node undecided"} />
        <HeaderStat icon="box" label="Model weights" value={formatBytes(endpoint.runtime?.model?.sizeBytes ?? model?.sizeBytes)} detail={shortID(endpoint.runtime?.model?.resolvedDigest ?? model?.digest, 18)} />
        <HeaderStat icon="clock" label="Last activity" value={formatRelativeTime(endpoint.runtime?.lastActivityTime)} detail={`zero after ${Math.round(endpoint.idleTimeoutSeconds / 60)}m`} />
      </section>

      <nav className="section-jump" aria-label="Endpoint sections">
        <a href="#lifecycle"><Icon name="activity" size={14} /> Lifecycle</a>
        <a href="#metrics"><Icon name="chart" size={14} /> Metrics</a>
        <a href="#inference"><Icon name="bot" size={14} /> Inference</a>
        <a href="#kubernetes"><Icon name="layers" size={14} /> Kubernetes</a>
        <a href="#evidence"><Icon name="terminal" size={14} /> Evidence</a>
      </nav>

      <div id="lifecycle">
        <LifecyclePanel endpoint={endpoint} observations={observations} />
      </div>
      {!isDeleted && (
        <>
          <MetricsPanel
            endpoint={endpoint}
            metrics={metrics}
            metricsError={metricsError}
            profile={profile}
            samples={samples.filter((sample) => sample.endpointID === endpoint.id)}
          />
          <InferenceLab
            endpoint={endpoint}
            metrics={metrics}
            onEvidenceChanged={() => {
              onRefresh();
              void refreshTelemetry();
              void refreshEvidence();
            }}
            onSample={addSample}
          />
          <InspectorPanel endpoint={endpoint} error={inspectionError} inspection={inspection} />
        </>
      )}
      <EvidencePanel
        endpoint={endpoint}
        error={evidenceError}
        events={events}
        logs={logs}
        onRefresh={() => void refreshEvidence()}
      />

      <DeleteDialog endpoint={endpoint} onClose={() => setDeleteOpen(false)} onDelete={onDelete} open={deleteOpen} />
    </div>
  );
}

function HeaderStat({
  icon,
  label,
  value,
  detail
}: {
  icon: Parameters<typeof Icon>[0]["name"];
  label: string;
  value: string;
  detail: string;
}) {
  return (
    <article>
      <span><Icon name={icon} size={17} /></span>
      <div><small>{label}</small><strong>{value}</strong><p>{detail}</p></div>
    </article>
  );
}
