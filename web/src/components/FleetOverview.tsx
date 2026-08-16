import type { Catalog, Endpoint } from "../types";
import { currentReason, formatBytes } from "../utils";
import { Icon } from "./Icon";
import { Button, Panel, PhaseBadge, Tag } from "./Primitives";

interface FleetOverviewProps {
  catalog: Catalog;
  endpoints: Endpoint[];
  onCreate: () => void;
  onSelect: (endpointID: string) => void;
}

export function FleetOverview({ catalog, endpoints, onCreate, onSelect }: FleetOverviewProps) {
  const active = endpoints.filter((endpoint) => !endpoint.deletedAt);
  const ready = active.filter((endpoint) => endpoint.runtime?.phase === "Ready");
  const scaledToZero = active.filter(
    (endpoint) => (endpoint.runtime?.replicas?.desired ?? 0) === 0 && endpoint.runtime?.phase === "Ready"
  );
  const allocatedGPUs = active.reduce((total, endpoint) => {
    const profile = catalog.profiles.find((candidate) => candidate.name === endpoint.profile);
    return total + (endpoint.runtime?.replicas?.desired ?? 0) * (profile?.gpuCount ?? 0);
  }, 0);
  const model = catalog.models[0];

  return (
    <div className="page fleet-page">
      <section className="fleet-hero">
        <div className="hero-copy">
          <div className="hero-kicker"><span /> Kubernetes GPU inference control plane</div>
          <h1>Ship an endpoint.<br /><em>Not a YAML stack.</em></h1>
          <p>
            Ember turns one reviewed model intent into a scheduled, cache-aware,
            autoscaled and reclaimable inference service.
          </p>
          <div className="hero-actions">
            <Button icon="spark" variant="primary" onClick={onCreate}>Launch endpoint</Button>
            <a className="text-link" href="#architecture">See the control loop <Icon name="arrow" size={15} /></a>
          </div>
          <div className="capability-row">
            <Tag tone="safe"><Icon name="shield" size={13} /> Owner isolated</Tag>
            <Tag><Icon name="gpu" size={13} /> Fake L4 in Kind</Tag>
            <Tag><Icon name="activity" size={13} /> KEDA + Prometheus</Tag>
            <Tag><Icon name="database" size={13} /> Durable audit</Tag>
          </div>
        </div>
        <div className="hero-visual" aria-label="Ember control loop visualization">
          <div className="orbit orbit-one" />
          <div className="orbit orbit-two" />
          <div className="ember-core">
            <span className="core-glow" />
            <Icon name="flame" size={34} />
            <strong>EMBER</strong>
            <small>reconcile loop</small>
          </div>
          <div className="orbit-node node-intent"><Icon name="code" /><span>Intent</span></div>
          <div className="orbit-node node-cache"><Icon name="database" /><span>Cache</span></div>
          <div className="orbit-node node-gpu"><Icon name="gpu" /><span>GPU</span></div>
          <div className="orbit-node node-scale"><Icon name="chart" /><span>Scale</span></div>
          <div className="visual-caption">
            <span className="live-dot" />
            Desired state continuously reconciled
          </div>
        </div>
      </section>

      <section className="fleet-stat-grid">
        <article className="fleet-stat">
          <span className="stat-icon"><Icon name="layers" /></span>
          <div><small>Endpoint records</small><strong>{endpoints.length}</strong></div>
          <span className="stat-caption">{active.length} live in Kubernetes</span>
        </article>
        <article className="fleet-stat">
          <span className="stat-icon stat-icon-positive"><Icon name="check" /></span>
          <div><small>Ready</small><strong>{ready.length}</strong></div>
          <span className="stat-caption">{scaledToZero.length} healthy at zero</span>
        </article>
        <article className="fleet-stat">
          <span className="stat-icon stat-icon-blue"><Icon name="gpu" /></span>
          <div><small>GPUs allocated now</small><strong>{allocatedGPUs}</strong></div>
          <span className="stat-caption">extended resources</span>
        </article>
        <article className="fleet-stat">
          <span className="stat-icon stat-icon-violet"><Icon name="database" /></span>
          <div><small>Catalog weight</small><strong>{formatBytes(model?.sizeBytes)}</strong></div>
          <span className="stat-caption">immutable revision</span>
        </article>
      </section>

      <section className="fleet-section">
        <div className="section-heading-row">
          <div>
            <span className="eyebrow">Inference fleet</span>
            <h2>Endpoint control cards</h2>
          </div>
          <Button icon="plus" onClick={onCreate}>New endpoint</Button>
        </div>
        {endpoints.length > 0 ? (
          <div className="fleet-cards">
            {endpoints.map((endpoint) => {
              const profile = catalog.profiles.find((candidate) => candidate.name === endpoint.profile);
              return (
                <button className="fleet-card" key={endpoint.id} onClick={() => onSelect(endpoint.id)}>
                  <div className="fleet-card-top">
                    <span className="endpoint-orb"><Icon name="gpu" /></span>
                    <PhaseBadge phase={endpoint.runtime?.phase} />
                  </div>
                  <h3>{endpoint.displayName}</h3>
                  <p>{endpoint.modelID}</p>
                  <div className="fleet-card-runtime">
                    <div><small>Replicas</small><strong>{endpoint.runtime?.replicas?.ready ?? 0}/{endpoint.runtime?.replicas?.desired ?? 0}</strong></div>
                    <div><small>GPU profile</small><strong>{profile?.gpuCount ?? 0}× L4</strong></div>
                    <div><small>Cache</small><strong>{endpoint.runtime?.placement?.cacheState ?? "pending"}</strong></div>
                  </div>
                  <div className="fleet-card-footer">
                    <span><span className="status-dot" /> {currentReason(endpoint)}</span>
                    <Icon name="arrow" size={16} />
                  </div>
                </button>
              );
            })}
          </div>
        ) : (
          <div className="fleet-empty">
            <div className="empty-blueprint">
              <Icon name="gpu" size={32} />
              <span className="blueprint-line line-one" />
              <span className="blueprint-line line-two" />
            </div>
            <h3>No endpoint intent yet</h3>
            <p>Create one to watch Ember generate and reconcile the complete Kubernetes serving stack.</p>
            <Button icon="spark" variant="primary" onClick={onCreate}>Create the first endpoint</Button>
          </div>
        )}
      </section>

      <Panel className="architecture-panel" eyebrow="What this project actually does" icon="network" id="architecture" title="One product action, nine Kubernetes mechanisms">
        <div className="control-loop">
          <ControlNode icon="spark" label="Product intent" detail="model + profile + policy" accent="orange" />
          <ControlArrow label="POST" />
          <ControlNode icon="code" label="InferenceEndpoint" detail="serving.ember.dev CR" accent="violet" />
          <ControlArrow label="reconcile" />
          <ControlNode icon="layers" label="Workload boundary" detail="Namespace · Quota · Policy" accent="blue" />
          <ControlArrow label="schedule" />
          <ControlNode icon="database" label="Warm cache" detail="ModelCache · node labels" accent="teal" />
          <ControlArrow label="serve" />
          <ControlNode icon="gpu" label="GPU engine" detail="Deployment · Service" accent="orange" />
          <ControlArrow label="observe" />
          <ControlNode icon="chart" label="Demand loop" detail="Prometheus · KEDA · HPA" accent="pink" />
        </div>
        <div className="mechanism-grid">
          {[
            ["GPU scheduling", "extended resources, selectors, taints and tolerations", "gpu"],
            ["Cache-aware placement", "verified safetensors, node labels and required affinity", "database"],
            ["Autoscaling", "queue-depth PromQL drives KEDA while Ember owns zero", "chart"],
            ["Reclamation", "finalizer waits for Pods and Namespace cleanup", "trash"],
            ["Isolation", "owner JWTs, dedicated namespaces and bounded read APIs", "shield"],
            ["Persistence", "Postgres retains metadata, idempotency and append-only audit", "database"]
          ].map(([title, detail, icon]) => (
            <article key={title}>
              <Icon name={icon as Parameters<typeof Icon>[0]["name"]} />
              <div><strong>{title}</strong><p>{detail}</p></div>
            </article>
          ))}
        </div>
        <div className="simulation-disclaimer">
          <Icon name="shield" size={17} />
          <span><strong>Honest demo boundary:</strong> the local cluster advertises fake L4 devices and runs a repository-owned mock engine. Scheduling and control-plane behavior are real; GPU throughput claims wait for GKE validation.</span>
        </div>
      </Panel>
    </div>
  );
}

function ControlNode({
  icon,
  label,
  detail,
  accent
}: {
  icon: Parameters<typeof Icon>[0]["name"];
  label: string;
  detail: string;
  accent: string;
}) {
  return (
    <div className={`control-node accent-${accent}`}>
      <span><Icon name={icon} /></span>
      <strong>{label}</strong>
      <small>{detail}</small>
    </div>
  );
}

function ControlArrow({ label }: { label: string }) {
  return (
    <div className="control-arrow">
      <small>{label}</small>
      <span><Icon name="arrow" size={18} /></span>
    </div>
  );
}
