import type { Endpoint, EndpointInspection, InspectionResource } from "../types";
import { formatRelativeTime } from "../utils";
import { Icon } from "./Icon";
import { Panel, Skeleton, Tag } from "./Primitives";

interface InspectorPanelProps {
  endpoint: Endpoint;
  inspection: EndpointInspection | null;
  error: string;
}

export function InspectorPanel({ endpoint, inspection, error }: InspectorPanelProps) {
  const resources = inspection?.resources ?? [];
  const resource = (kind: string) => resources.find((item) => item.kind === kind);
  const deployment = resource("Deployment");
  const scaledObject = resource("ScaledObject");
  const hpa = resource("HorizontalPodAutoscaler");
  const quota = resource("ResourceQuota");
  const service = resource("Service");

  return (
    <Panel
      action={
        inspection ? (
          <Tag tone="live"><span className="live-dot" /> observed {formatRelativeTime(inspection.observedAt)}</Tag>
        ) : (
          <Tag tone={error ? "warning" : "neutral"}>{error || "Loading cluster state"}</Tag>
        )
      }
      className="inspector-panel"
      eyebrow="Generated child resources"
      icon="layers"
      id="kubernetes"
      title="Kubernetes runtime inspector"
    >
      <div className="mechanism-strip">
        {[
          ["Namespace", "isolation", "layers"],
          ["ResourceQuota", "GPU bounds", "shield"],
          ["Node affinity", "warm cache", "database"],
          ["Tolerations", "GPU taint", "gpu"],
          ["Deployment", "self-healing", "box"],
          ["Service", "routing", "network"],
          ["KEDA + HPA", "queue scaling", "chart"],
          ["Finalizer", "safe cleanup", "trash"]
        ].map(([title, detail, icon]) => (
          <div key={title}>
            <Icon name={icon as Parameters<typeof Icon>[0]["name"]} size={15} />
            <span><strong>{title}</strong><small>{detail}</small></span>
          </div>
        ))}
      </div>

      {inspection ? (
        <>
          <div className="topology-canvas">
            <TopologyNode
              accent="violet"
              detail={endpoint.id}
              icon="code"
              label="InferenceEndpoint"
              status={endpoint.runtime?.phase ?? "Creating"}
            />
            <TopologyConnector label="finalizer + UID labels" />
            <div className="topology-group">
              <div className="namespace-label">
                <Icon name="layers" size={14} />
                {inspection.namespace}
              </div>
              <div className="topology-row">
                <TopologyNode accent="blue" detail={quota?.summary ?? "resource bounds"} icon="shield" label="Quota" status={quota?.status ?? "Pending"} />
                <TopologyNode accent="orange" detail={deployment?.summary ?? "engine rollout"} icon="box" label="Deployment" status={deployment?.status ?? "Pending"} />
                <TopologyNode accent="teal" detail={service?.summary ?? "port 8000"} icon="network" label="Service" status={service?.status ?? "Pending"} />
              </div>
              <div className="topology-row topology-row-secondary">
                <TopologyNode accent="pink" detail={scaledObject?.summary ?? "Prometheus trigger"} icon="activity" label="ScaledObject" status={scaledObject?.status ?? "Pending"} />
                <TopologyNode accent="blue" detail={hpa?.summary ?? "KEDA-owned HPA"} icon="chart" label="HPA" status={hpa?.status ?? "Pending"} />
                <TopologyNode
                  accent="orange"
                  detail={`${inspection.pods.length} observed · ${inspection.pods.reduce((sum, pod) => sum + pod.requestedGPUs, 0)} GPU`}
                  icon="gpu"
                  label="Engine Pods"
                  status={`${inspection.pods.filter((pod) => pod.ready).length} ready`}
                />
              </div>
            </div>
          </div>

          <div className="inspector-grid">
            <div className="resource-inventory">
              <div className="subsection-heading">
                <div><span className="eyebrow">Live API reads</span><h3>Resource inventory</h3></div>
                <small>{resources.length + inspection.pods.length} objects projected</small>
              </div>
              <div className="resource-table">
                <div className="resource-table-head"><span>Kind / name</span><span>Status</span><span>Evidence</span></div>
                {resources.map((item) => (
                  <div className="resource-table-row" key={`${item.kind}-${item.name}`}>
                    <span><ResourceIcon resource={item} /><span><strong>{item.kind}</strong><small>{item.name}</small></span></span>
                    <Tag tone={statusTone(item.status)}>{item.status}</Tag>
                    <code>{item.summary}</code>
                  </div>
                ))}
                {inspection.pods.map((pod) => (
                  <div className="resource-table-row" key={pod.name}>
                    <span><span className="resource-kind-icon accent-orange"><Icon name="gpu" size={14} /></span><span><strong>Pod</strong><small>{pod.name}</small></span></span>
                    <Tag tone={pod.ready ? "safe" : "warning"}>{pod.ready ? "Ready" : pod.phase}</Tag>
                    <code>{pod.node || "unscheduled"} · {pod.requestedGPUs} GPU · restarts {pod.restartCount}</code>
                  </div>
                ))}
              </div>
            </div>

            <div className="security-posture">
              <div className="subsection-heading">
                <div><span className="eyebrow">Derived from PodSpec + policy</span><h3>Security posture</h3></div>
              </div>
              <div className="security-list">
                {inspection.securityControls.map((control) => (
                  <article className={`security-control state-${control.state}`} key={control.name}>
                    <span className="security-state-icon">
                      <Icon
                        name={control.state === "pass" ? "check" : control.state === "warning" ? "shield" : "clock"}
                        size={14}
                      />
                    </span>
                    <div><strong>{control.name}</strong><p>{control.evidence}</p></div>
                    <Tag tone={control.state === "pass" ? "safe" : control.state === "warning" ? "warning" : "neutral"}>
                      {control.state}
                    </Tag>
                  </article>
                ))}
              </div>
            </div>
          </div>

          <div className="policy-and-events">
            <div className="policy-list">
              <div className="subsection-heading">
                <div><span className="eyebrow">Declared network intent</span><h3>NetworkPolicies</h3></div>
              </div>
              {inspection.networkPolicies.map((policy) => (
                <article key={policy.name}>
                  <span><Icon name="network" size={15} /></span>
                  <div><strong>{policy.name}</strong><small>{policy.policyTypes.join(" + ")}</small></div>
                  <code>in {policy.ingressRules} · out {policy.egressRules}</code>
                </article>
              ))}
            </div>
            <div className="kube-event-list">
              <div className="subsection-heading">
                <div><span className="eyebrow">Namespace event stream</span><h3>Recent Kubernetes events</h3></div>
              </div>
              {inspection.events.length > 0 ? inspection.events.slice(0, 8).map((event, index) => (
                <article key={`${event.objectName}-${event.reason}-${index}`}>
                  <span className={event.type === "Warning" ? "event-dot warning" : "event-dot"} />
                  <div><strong>{event.reason}</strong><p>{event.message}</p><small>{event.objectKind}/{event.objectName}</small></div>
                  <time>{formatRelativeTime(event.lastSeen)}</time>
                </article>
              )) : <div className="quiet-empty">No namespace events are currently retained.</div>}
            </div>
          </div>
        </>
      ) : (
        <div className="inspector-loading">
          <Skeleton className="topology-skeleton" />
          <div><Skeleton /><Skeleton /><Skeleton /></div>
          {error && <p>{error}</p>}
        </div>
      )}

      <div className="truth-boundary">
        <Icon name="shield" size={16} />
        <span>The inspector is owner-scoped and read-only. It projects a fixed allowlist of workload resources and never exposes Secrets, tokens or arbitrary Kubernetes API access.</span>
      </div>
    </Panel>
  );
}

function TopologyNode({
  icon,
  label,
  detail,
  status,
  accent
}: {
  icon: Parameters<typeof Icon>[0]["name"];
  label: string;
  detail: string;
  status: string;
  accent: string;
}) {
  return (
    <div className={`topology-node accent-${accent}`}>
      <span className="topology-node-icon"><Icon name={icon} size={17} /></span>
      <div><strong>{label}</strong><small>{detail}</small></div>
      <Tag tone={statusTone(status)}>{status}</Tag>
    </div>
  );
}

function TopologyConnector({ label }: { label: string }) {
  return <div className="topology-connector"><span /><small>{label}</small><Icon name="arrow" size={17} /></div>;
}

function ResourceIcon({ resource }: { resource: InspectionResource }) {
  const iconMap: Record<string, Parameters<typeof Icon>[0]["name"]> = {
    Namespace: "layers",
    ResourceQuota: "shield",
    LimitRange: "cpu",
    Deployment: "box",
    Service: "network",
    ScaledObject: "activity",
    HorizontalPodAutoscaler: "chart"
  };
  return <span className="resource-kind-icon"><Icon name={iconMap[resource.kind] ?? "code"} size={14} /></span>;
}

function statusTone(status: string): string {
  const normalized = status.toLowerCase();
  if (normalized.includes("ready") || normalized.includes("active") || normalized.includes("enforced") || normalized.includes("routable") || normalized.includes("watching")) {
    return "safe";
  }
  if (normalized.includes("pending") || normalized.includes("progress") || normalized.includes("paused")) {
    return "warning";
  }
  return "neutral";
}
