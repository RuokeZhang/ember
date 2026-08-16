import { useCallback, useEffect, useMemo, useState } from "react";

import {
  bootstrapSession,
  createEndpoint,
  deleteEndpoint,
  getCatalog,
  getEndpoint,
  listEndpoints
} from "./api";
import { CreateEndpointDrawer } from "./components/CreateEndpointDrawer";
import { EndpointDashboard } from "./components/EndpointDashboard";
import { FleetOverview } from "./components/FleetOverview";
import { Icon } from "./components/Icon";
import { Button, IconButton, PhaseBadge, Spinner } from "./components/Primitives";
import { useEndpointStream } from "./hooks";
import type { Catalog, CreateEndpointInput, Endpoint, Session } from "./types";
import { currentReason, shortID } from "./utils";

function endpointFromPath(): string | null {
  const match = window.location.pathname.match(/^\/endpoints\/([^/]+)$/);
  return match ? decodeURIComponent(match[1]) : null;
}

export default function App() {
  const [session, setSession] = useState<Session | null>(null);
  const [catalog, setCatalog] = useState<Catalog | null>(null);
  const [endpoints, setEndpoints] = useState<Endpoint[]>([]);
  const [selectedID, setSelectedID] = useState<string | null>(() => endpointFromPath());
  const [createOpen, setCreateOpen] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [toast, setToast] = useState("");

  const selected = useMemo(
    () => endpoints.find((endpoint) => endpoint.id === selectedID),
    [endpoints, selectedID]
  );

  const refreshEndpoints = useCallback(async (quiet = false) => {
    try {
      const next = await listEndpoints();
      setEndpoints(next);
      if (!quiet) {
        setError("");
      }
    } catch (refreshError) {
      if (!quiet) {
        setError(refreshError instanceof Error ? refreshError.message : "Could not load endpoints");
      }
    }
  }, []);

  useEffect(() => {
    let active = true;
    Promise.all([bootstrapSession(), getCatalog(), listEndpoints()])
      .then(([nextSession, nextCatalog, nextEndpoints]) => {
        if (!active) {
          return;
        }
        setSession(nextSession);
        setCatalog(nextCatalog);
        setEndpoints(nextEndpoints);
        setError("");
      })
      .catch((bootstrapError) => {
        if (active) {
          setError(bootstrapError instanceof Error ? bootstrapError.message : "Ember could not start");
        }
      })
      .finally(() => {
        if (active) {
          setLoading(false);
        }
      });
    return () => {
      active = false;
    };
  }, []);

  useEffect(() => {
    if (!session) {
      return;
    }
    const timer = window.setInterval(() => void refreshEndpoints(true), 5000);
    return () => window.clearInterval(timer);
  }, [refreshEndpoints, session]);

  useEffect(() => {
    const onPopState = () => setSelectedID(endpointFromPath());
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

  useEffect(() => {
    if (!toast) {
      return;
    }
    const timer = window.setTimeout(() => setToast(""), 3200);
    return () => window.clearTimeout(timer);
  }, [toast]);

  useEndpointStream(
    selected,
    (updated) => {
      setEndpoints((current) => current.map((endpoint) => (endpoint.id === updated.id ? updated : endpoint)));
    },
    () => void refreshEndpoint(selectedID)
  );

  const navigate = (endpointID: string | null) => {
    const path = endpointID ? `/endpoints/${encodeURIComponent(endpointID)}` : "/";
    window.history.pushState({}, "", path);
    setSelectedID(endpointID);
  };

  const refreshEndpoint = useCallback(
    async (endpointID: string | null = selectedID) => {
      if (!endpointID) {
        await refreshEndpoints(true);
        return;
      }
      try {
        const updated = await getEndpoint(endpointID);
        setEndpoints((current) => {
          const exists = current.some((endpoint) => endpoint.id === endpointID);
          return exists
            ? current.map((endpoint) => (endpoint.id === endpointID ? updated : endpoint))
            : [updated, ...current];
        });
      } catch {
        await refreshEndpoints(true);
      }
    },
    [refreshEndpoints, selectedID]
  );

  const handleCreate = async (input: CreateEndpointInput, idempotencyKey: string) => {
    const endpoint = await createEndpoint(input, idempotencyKey);
    setEndpoints((current) => [endpoint, ...current.filter((item) => item.id !== endpoint.id)]);
    setCreateOpen(false);
    navigate(endpoint.id);
    setToast("Endpoint intent accepted. Ember is reconciling Kubernetes resources.");
  };

  const handleDelete = async (endpointID: string) => {
    const updated = await deleteEndpoint(endpointID);
    setEndpoints((current) => current.map((endpoint) => (endpoint.id === endpointID ? updated : endpoint)));
    setToast("Deletion accepted. Ember will retain metadata after Kubernetes cleanup.");
    await refreshEndpoint(endpointID);
  };

  if (loading) {
    return (
      <main className="boot-screen">
        <div className="brand-mark brand-mark-large">
          <Icon name="flame" size={28} />
        </div>
        <h1>Starting Ember</h1>
        <Spinner label="Connecting to the control plane" />
      </main>
    );
  }

  if (!session || !catalog) {
    return (
      <main className="boot-screen">
        <div className="brand-mark brand-mark-large">
          <Icon name="flame" size={28} />
        </div>
        <h1>Control plane unavailable</h1>
        <p>{error || "The demo session could not be initialized."}</p>
        <Button variant="primary" icon="refresh" onClick={() => window.location.reload()}>
          Retry
        </Button>
      </main>
    );
  }

  return (
    <div className="app-shell">
      <aside className="icon-rail">
        <button aria-label="Open fleet" className="brand-mark" onClick={() => navigate(null)}>
          <Icon name="flame" size={22} />
        </button>
        <nav className="rail-nav" aria-label="Primary navigation">
          <button className={!selectedID ? "rail-item active" : "rail-item"} onClick={() => navigate(null)}>
            <Icon name="grid" />
            <span>Fleet</span>
          </button>
          <a className="rail-item" href={selectedID ? "#metrics" : "#architecture"}>
            <Icon name="chart" />
            <span>Metrics</span>
          </a>
          <a className="rail-item" href={selectedID ? "#kubernetes" : "#architecture"}>
            <Icon name="layers" />
            <span>Kubernetes</span>
          </a>
          <a className="rail-item" href={selectedID ? "#inference" : "#architecture"}>
            <Icon name="bot" />
            <span>Inference</span>
          </a>
        </nav>
        <div className="rail-footer" title={`Demo owner ${session.ownerID}`}>
          <span className="owner-avatar">{session.ownerID.slice(-2).toUpperCase()}</span>
        </div>
      </aside>

      <aside className="endpoint-sidebar">
        <div className="sidebar-heading">
          <div>
            <span className="eyebrow">Control plane</span>
            <strong>Endpoints</strong>
          </div>
          <IconButton icon="plus" label="Create endpoint" onClick={() => setCreateOpen(true)} />
        </div>
        <button className={!selectedID ? "fleet-link active" : "fleet-link"} onClick={() => navigate(null)}>
          <span className="fleet-icon"><Icon name="grid" size={15} /></span>
          <span>
            <strong>Fleet overview</strong>
            <small>{endpoints.filter((endpoint) => !endpoint.deletedAt).length} active records</small>
          </span>
          <Icon name="chevron" size={14} />
        </button>
        <div className="endpoint-list">
          {endpoints.map((endpoint) => (
            <button
              className={selectedID === endpoint.id ? "endpoint-item active" : "endpoint-item"}
              key={endpoint.id}
              onClick={() => navigate(endpoint.id)}
            >
              <span className={`endpoint-orb tone-${endpoint.runtime?.phase?.toLowerCase() ?? "creating"}`}>
                <Icon name="gpu" size={15} />
              </span>
              <span className="endpoint-item-copy">
                <strong>{endpoint.displayName}</strong>
                <small>{currentReason(endpoint)}</small>
              </span>
              <PhaseBadge phase={endpoint.runtime?.phase} />
            </button>
          ))}
          {endpoints.length === 0 && (
            <div className="sidebar-empty">
              <Icon name="gpu" size={22} />
              <span>No endpoints yet</span>
            </div>
          )}
        </div>
        <div className="sidebar-session">
          <span className="session-live"><span /> Session live</span>
          <code>{shortID(session.ownerID, 15)}</code>
        </div>
      </aside>

      <main className="workspace">
        <header className="workspace-topbar">
          <div className="environment-pill">
            <span className="live-dot" />
            Kubernetes connected
          </div>
          <div className="topbar-actions">
            {error && <span className="topbar-error">{error}</span>}
            <Button icon="refresh" variant="ghost" onClick={() => void refreshEndpoints()}>
              Sync
            </Button>
            <Button icon="plus" variant="primary" onClick={() => setCreateOpen(true)}>
              New endpoint
            </Button>
          </div>
        </header>

        {selected ? (
          <EndpointDashboard
            catalog={catalog}
            endpoint={selected}
            onDelete={handleDelete}
            onRefresh={() => void refreshEndpoint(selected.id)}
          />
        ) : (
          <FleetOverview
            catalog={catalog}
            endpoints={endpoints}
            onCreate={() => setCreateOpen(true)}
            onSelect={(endpointID) => navigate(endpointID)}
          />
        )}
      </main>

      <CreateEndpointDrawer
        catalog={catalog}
        onClose={() => setCreateOpen(false)}
        onCreate={handleCreate}
        open={createOpen}
      />
      {toast && <div className="toast"><Icon name="check" size={17} />{toast}</div>}
    </div>
  );
}
