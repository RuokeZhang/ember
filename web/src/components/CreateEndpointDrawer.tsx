import { useEffect, useMemo, useState } from "react";

import { APIError } from "../api";
import type { Catalog, CreateEndpointInput } from "../types";
import { formatBytes, stableIdempotencyKey } from "../utils";
import { Icon } from "./Icon";
import { Button, InlineError, Tag } from "./Primitives";

interface CreateEndpointDrawerProps {
  catalog: Catalog;
  open: boolean;
  onClose: () => void;
  onCreate: (input: CreateEndpointInput, idempotencyKey: string) => Promise<void>;
}

const defaultInput: CreateEndpointInput = {
  displayName: "Qwen reasoning endpoint",
  modelID: "qwen2.5-7b-instruct-awq",
  profile: "standard",
  minReplicas: 0,
  maxReplicas: 3,
  targetQueueDepth: 4,
  idleTimeoutSeconds: 120,
  cachePreference: "Preferred",
  maxColdStartFallbackSeconds: 120
};

export function CreateEndpointDrawer({ catalog, open, onClose, onCreate }: CreateEndpointDrawerProps) {
  const [input, setInput] = useState<CreateEndpointInput>(defaultInput);
  const [idempotencyKey, setIdempotencyKey] = useState(() => stableIdempotencyKey());
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");

  const model = useMemo(
    () => catalog.models.find((candidate) => candidate.id === input.modelID) ?? catalog.models[0],
    [catalog.models, input.modelID]
  );
  const profiles = catalog.profiles.filter((profile) => model?.allowedProfiles.includes(profile.name));
  const selectedProfile = profiles.find((profile) => profile.name === input.profile) ?? profiles[0];

  useEffect(() => {
    if (!open) {
      return;
    }
    if (model && !model.allowedProfiles.includes(input.profile) && profiles[0]) {
      setInput((current) => ({ ...current, profile: profiles[0].name }));
    }
  }, [input.profile, model, open, profiles]);

  const update = <Key extends keyof CreateEndpointInput>(key: Key, value: CreateEndpointInput[Key]) => {
    setInput((current) => ({ ...current, [key]: value }));
  };

  const submit = async () => {
    setSubmitting(true);
    setError("");
    try {
      await onCreate(input, idempotencyKey);
      setInput(defaultInput);
      setIdempotencyKey(stableIdempotencyKey());
    } catch (submitError) {
      const message =
        submitError instanceof APIError
          ? `${submitError.message}${submitError.code === "idempotency_conflict" ? " Generate a new intent before retrying." : ""}`
          : submitError instanceof Error
            ? submitError.message
            : "Endpoint could not be created";
      setError(message);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <>
      <div className={open ? "drawer-backdrop visible" : "drawer-backdrop"} onClick={onClose} />
      <aside aria-hidden={!open} className={open ? "create-drawer open" : "create-drawer"}>
        <header className="drawer-header">
          <div>
            <span className="eyebrow">Declare serving intent</span>
            <h2>Launch inference endpoint</h2>
          </div>
          <button aria-label="Close create panel" className="drawer-close" onClick={onClose}>
            <Icon name="close" />
          </button>
        </header>

        <div className="drawer-body">
          <label className="field">
            <span>Endpoint name</span>
            <input
              maxLength={80}
              value={input.displayName}
              onChange={(event) => update("displayName", event.target.value)}
            />
          </label>

          <div className="form-section">
            <div className="section-number">01</div>
            <div className="form-section-heading">
              <div>
                <span className="eyebrow">Allowlisted artifact</span>
                <h3>Model</h3>
              </div>
              <Tag tone="safe"><Icon name="shield" size={13} /> Reviewed</Tag>
            </div>
            <div className="model-selector">
              {catalog.models.map((candidate) => (
                <button
                  className={candidate.id === input.modelID ? "model-option selected" : "model-option"}
                  key={candidate.id}
                  onClick={() => update("modelID", candidate.id)}
                  type="button"
                >
                  <span className="model-glyph"><Icon name="spark" /></span>
                  <span>
                    <strong>{candidate.id}</strong>
                    <small>{formatBytes(candidate.sizeBytes)} · AWQ · rev {candidate.revision}</small>
                  </span>
                  <span className="selection-ring"><Icon name="check" size={13} /></span>
                </button>
              ))}
            </div>
            {model && (
              <dl className="artifact-facts">
                <div><dt>Engine</dt><dd>{model.engineImage}</dd></div>
                <div><dt>Digest</dt><dd><code>{model.digest.slice(0, 24)}…</code></dd></div>
                <div><dt>Revision</dt><dd><code>{model.revision}</code></dd></div>
              </dl>
            )}
          </div>

          <div className="form-section">
            <div className="section-number">02</div>
            <div className="form-section-heading">
              <div>
                <span className="eyebrow">Resource contract</span>
                <h3>Serving profile</h3>
              </div>
            </div>
            <div className="profile-grid">
              {profiles.map((profile) => (
                <button
                  className={profile.name === input.profile ? "profile-option selected" : "profile-option"}
                  key={profile.name}
                  onClick={() => update("profile", profile.name)}
                  type="button"
                >
                  <span className="profile-topline">
                    <Icon name="gpu" size={17} />
                    <strong>{profile.name}</strong>
                  </span>
                  <b>{profile.gpuCount}× L4</b>
                  <small>{profile.cpuRequest} CPU · {profile.memoryRequest} RAM</small>
                </button>
              ))}
            </div>
          </div>

          <div className="form-section">
            <div className="section-number">03</div>
            <div className="form-section-heading">
              <div>
                <span className="eyebrow">Runtime policy</span>
                <h3>Autoscaling & placement</h3>
              </div>
            </div>
            <div className="range-field">
              <div><span>Maximum replicas</span><strong>{input.maxReplicas}</strong></div>
              <input
                max={10}
                min={1}
                type="range"
                value={input.maxReplicas}
                onChange={(event) => update("maxReplicas", Number(event.target.value))}
              />
              <small>KEDA scales from 1 to {input.maxReplicas}; Ember owns idle scale-to-zero.</small>
            </div>
            <div className="range-field">
              <div><span>Target queue depth</span><strong>{input.targetQueueDepth}</strong></div>
              <input
                max={32}
                min={1}
                type="range"
                value={input.targetQueueDepth}
                onChange={(event) => update("targetQueueDepth", Number(event.target.value))}
              />
              <small>Each replica should absorb roughly {input.targetQueueDepth} waiting requests.</small>
            </div>
            <div className="two-column-fields">
              <label className="field">
                <span>Idle timeout</span>
                <select
                  value={input.idleTimeoutSeconds}
                  onChange={(event) => update("idleTimeoutSeconds", Number(event.target.value))}
                >
                  <option value={60}>1 minute</option>
                  <option value={120}>2 minutes</option>
                  <option value={300}>5 minutes</option>
                  <option value={900}>15 minutes</option>
                </select>
              </label>
              <label className="field">
                <span>Warm cache</span>
                <select
                  value={input.cachePreference}
                  onChange={(event) => update("cachePreference", event.target.value as "Preferred" | "Required")}
                >
                  <option value="Preferred">Preferred</option>
                  <option value="Required">Required</option>
                </select>
              </label>
            </div>
          </div>

          <div className="intent-preview">
            <div className="intent-preview-heading">
              <span><Icon name="code" size={15} /> Product intent</span>
              <Tag>server validated</Tag>
            </div>
            <code>
              {`model: ${input.modelID}\nprofile: ${input.profile}\ngpu: ${selectedProfile?.gpuCount ?? 0}× nvidia.com/gpu\nscale: 0 → ${input.maxReplicas} @ queue ${input.targetQueueDepth}\ncache: ${input.cachePreference}`}
            </code>
            <small>Ember—not the browser—derives the Namespace, Quota, Deployment, Service, NetworkPolicies and KEDA objects.</small>
          </div>

          <div className="idempotency-note">
            <Icon name="shield" size={15} />
            <span>
              Safe retry key
              <code>{idempotencyKey}</code>
            </span>
          </div>
          {error && <InlineError message={error} />}
        </div>

        <footer className="drawer-footer">
          <Button disabled={submitting} variant="ghost" onClick={onClose}>Cancel</Button>
          <Button disabled={submitting || !input.displayName.trim()} icon="spark" variant="primary" onClick={() => void submit()}>
            {submitting ? "Submitting intent…" : "Create endpoint"}
          </Button>
        </footer>
      </aside>
    </>
  );
}
