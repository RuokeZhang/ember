import { useEffect } from "react";

import { useStoredState } from "../hooks";
import type { CatalogProfile, Endpoint, EndpointMetrics, PerformanceSample } from "../types";
import { chartPolyline, formatDuration } from "../utils";
import { Icon } from "./Icon";
import { Panel, Skeleton, Tag } from "./Primitives";

interface MetricsPanelProps {
  endpoint: Endpoint;
  metrics: EndpointMetrics | null;
  metricsError: string;
  profile?: CatalogProfile;
  samples: PerformanceSample[];
}

interface GPUMeter {
  gpuSeconds: number;
  idleGPUSeconds: number;
  lastObservedAt: string;
}

export function MetricsPanel({ endpoint, metrics, metricsError, profile, samples }: MetricsPanelProps) {
  const [meter, setMeter] = useStoredState<GPUMeter>(`ember.gpu-meter.${endpoint.id}`, {
    gpuSeconds: 0,
    idleGPUSeconds: 0,
    lastObservedAt: ""
  });
  const gpuCount = profile?.gpuCount ?? 0;

  useEffect(() => {
    if (!metrics?.observedAt) {
      return;
    }
    setMeter((current) => {
      const observedAt = new Date(metrics.observedAt).getTime();
      const previousAt = current.lastObservedAt ? new Date(current.lastObservedAt).getTime() : observedAt;
      const elapsed = Math.max(0, Math.min(15, (observedAt - previousAt) / 1000));
      const allocated = metrics.current.replicas * gpuCount;
      return {
        gpuSeconds: current.gpuSeconds + elapsed * allocated,
        idleGPUSeconds:
          current.idleGPUSeconds + (metrics.current.runningRequests === 0 ? elapsed * allocated : 0),
        lastObservedAt: metrics.observedAt
      };
    });
  }, [gpuCount, metrics?.observedAt, metrics?.current.replicas, metrics?.current.runningRequests, setMeter]);

  const points = metrics?.series ?? [];
  const maxValue = Math.max(
    1,
    ...points.map((point) => Math.max(point.queueDepth, point.runningRequests, point.replicas))
  );
  const activationSamples = samples.filter((sample) => sample.kind === "activation");
  const warmSamples = samples.filter((sample) => sample.kind === "warm");
  const activationAverage = average(activationSamples.map((sample) => sample.ttftMs));
  const warmAverage = average(warmSamples.map((sample) => sample.ttftMs));
  const maxTTFT = Math.max(activationAverage, warmAverage, 1);
  const allocatedNow = (endpoint.runtime?.replicas?.desired ?? 0) * gpuCount;
  const idlePercent = meter.gpuSeconds > 0 ? (meter.idleGPUSeconds / meter.gpuSeconds) * 100 : 0;

  return (
    <Panel
      action={<Tag tone={metricsError ? "warning" : "live"}>{metricsError ? "Telemetry delayed" : "Prometheus · 5s"}</Tag>}
      className="metrics-panel"
      eyebrow="Demand → capacity"
      icon="chart"
      id="metrics"
      title="Queue depth and replica response"
    >
      <div className="metric-summary-row">
        <MetricValue
          accent="orange"
          icon="activity"
          label="Queue depth"
          value={metrics ? numberValue(metrics.current.queueDepth) : "—"}
          detail={`target ${endpoint.targetQueueDepth} / replica`}
        />
        <MetricValue
          accent="pink"
          icon="bot"
          label="Running requests"
          value={metrics ? numberValue(metrics.current.runningRequests) : "—"}
          detail={`${numberValue(metrics?.current.requestsTotal ?? 0)} accepted total`}
        />
        <MetricValue
          accent="blue"
          icon="layers"
          label="Scraped replicas"
          value={metrics ? numberValue(metrics.current.replicas) : "—"}
          detail={`CR desired ${endpoint.runtime?.replicas?.desired ?? 0}`}
        />
        <MetricValue
          accent="violet"
          icon="gpu"
          label="GPUs allocated"
          value={allocatedNow.toString()}
          detail={`${gpuCount} per replica · ${profile?.name ?? endpoint.profile}`}
        />
      </div>

      <div className="chart-and-cost">
        <div className="timeseries-card">
          <div className="chart-legend">
            <span><i className="legend-orange" /> Queue</span>
            <span><i className="legend-pink" /> Running</span>
            <span><i className="legend-blue" /> Replicas</span>
            <small>last {metrics ? Math.round(metrics.windowSeconds / 60) : 15} min</small>
          </div>
          {points.length > 1 ? (
            <svg aria-label="Queue depth and replica time series" className="metric-chart" preserveAspectRatio="none" viewBox="0 0 800 220">
              <defs>
                <linearGradient id="queue-fill" x1="0" x2="0" y1="0" y2="1">
                  <stop offset="0%" stopColor="#ff7a45" stopOpacity=".28" />
                  <stop offset="100%" stopColor="#ff7a45" stopOpacity="0" />
                </linearGradient>
              </defs>
              {[0, 1, 2, 3, 4].map((line) => (
                <line className="chart-gridline" key={line} x1="0" x2="800" y1={line * 55} y2={line * 55} />
              ))}
              <polygon
                fill="url(#queue-fill)"
                points={`0,220 ${chartPolyline(points, "queueDepth", 800, 200, maxValue)} 800,220`}
              />
              <polyline className="line-queue" points={chartPolyline(points, "queueDepth", 800, 200, maxValue)} />
              <polyline className="line-running" points={chartPolyline(points, "runningRequests", 800, 200, maxValue)} />
              <polyline className="line-replicas" points={chartPolyline(points, "replicas", 800, 200, maxValue)} />
            </svg>
          ) : metricsError ? (
            <div className="chart-empty"><Icon name="activity" /><span>{metricsError}</span></div>
          ) : (
            <div className="chart-loading">
              <Skeleton className="chart-skeleton" />
              <span>Waiting for Prometheus samples</span>
            </div>
          )}
          <div className="chart-axis-labels"><span>−15m</span><span>−10m</span><span>−5m</span><span>now</span></div>
        </div>

        <div className="gpu-meter-card">
          <div className="gpu-meter-heading">
            <span className="stat-icon stat-icon-blue"><Icon name="gpu" /></span>
            <div><span className="eyebrow">Browser-observed</span><h3>GPU allocation meter</h3></div>
          </div>
          <div className="donut-wrap">
            <div
              className="donut"
              style={{ "--idle-percent": `${Math.min(100, idlePercent)}%` } as React.CSSProperties}
            >
              <div><strong>{formatGPUSeconds(meter.gpuSeconds)}</strong><small>GPU time</small></div>
            </div>
            <div className="donut-legend">
              <div><span className="legend-swatch used" /><span>Request-active</span><strong>{formatGPUSeconds(meter.gpuSeconds - meter.idleGPUSeconds)}</strong></div>
              <div><span className="legend-swatch idle" /><span>Observed idle</span><strong>{formatGPUSeconds(meter.idleGPUSeconds)}</strong></div>
            </div>
          </div>
          <p>This meter integrates live replica samples in this browser. It is evidence for the UI, not a billing claim.</p>
        </div>
      </div>

      <div className="latency-compare">
        <div className="latency-copy">
          <span className="eyebrow">Inference evidence</span>
          <h3>Activation vs warm TTFT</h3>
          <p>Samples are measured at the browser through Control API and Gateway, including activation, proxy and queue time.</p>
        </div>
        <LatencyBar
          count={activationSamples.length}
          label="Scale-from-zero"
          max={maxTTFT}
          tone="activation"
          value={activationAverage}
        />
        <LatencyBar count={warmSamples.length} label="Warm replica" max={maxTTFT} tone="warm" value={warmAverage} />
      </div>
    </Panel>
  );
}

function MetricValue({
  accent,
  icon,
  label,
  value,
  detail
}: {
  accent: string;
  icon: Parameters<typeof Icon>[0]["name"];
  label: string;
  value: string;
  detail: string;
}) {
  return (
    <article className={`metric-value accent-${accent}`}>
      <span><Icon name={icon} size={16} /></span>
      <div><small>{label}</small><strong>{value}</strong><p>{detail}</p></div>
    </article>
  );
}

function LatencyBar({
  label,
  value,
  max,
  count,
  tone
}: {
  label: string;
  value: number;
  max: number;
  count: number;
  tone: string;
}) {
  const width = count ? Math.max(8, (value / max) * 100) : 0;
  return (
    <div className="latency-row">
      <div><strong>{label}</strong><small>{count} sample{count === 1 ? "" : "s"}</small></div>
      <div className="latency-track"><span className={`latency-fill ${tone}`} style={{ width: `${width}%` }} /></div>
      <b>{count ? formatDuration(value) : "run chat"}</b>
    </div>
  );
}

function average(values: number[]): number {
  return values.length ? values.reduce((sum, value) => sum + value, 0) / values.length : 0;
}

function numberValue(value: number): string {
  return Number.isInteger(value) ? value.toString() : value.toFixed(1);
}

function formatGPUSeconds(value: number): string {
  if (value < 60) {
    return `${Math.round(value)}s`;
  }
  return `${(value / 60).toFixed(1)}m`;
}
