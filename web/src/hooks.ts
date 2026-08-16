import { useEffect, useRef, useState } from "react";

import type { Endpoint, EndpointPhase, PhaseObservation } from "./types";
import { currentReason, mergeEndpointRuntime, recordPhaseObservation } from "./utils";

export function useStoredState<T>(key: string, initialValue: T): [T, (value: T | ((current: T) => T)) => void] {
  const [value, setValue] = useState<T>(() => {
    try {
      const stored = localStorage.getItem(key);
      return stored ? (JSON.parse(stored) as T) : initialValue;
    } catch {
      return initialValue;
    }
  });

  const update = (next: T | ((current: T) => T)) => {
    setValue((current) => {
      const resolved = typeof next === "function" ? (next as (value: T) => T)(current) : next;
      try {
        localStorage.setItem(key, JSON.stringify(resolved));
      } catch {
        // The UI still works when storage is disabled; only browser-observed history is lost.
      }
      return resolved;
    });
  };
  return [value, update];
}

export function useEndpointStream(
  endpoint: Endpoint | undefined,
  onUpdate: (endpoint: Endpoint) => void,
  onDeleted: () => void
): void {
  const updateRef = useRef(onUpdate);
  const deletedRef = useRef(onDeleted);
  updateRef.current = onUpdate;
  deletedRef.current = onDeleted;

  useEffect(() => {
    if (!endpoint || endpoint.deletedAt || !endpoint.runtime?.uid) {
      return;
    }
    const source = new EventSource(`/api/v1/endpoints/${encodeURIComponent(endpoint.id)}/stream`);
    source.addEventListener("status", (event) => {
      try {
        const raw = JSON.parse((event as MessageEvent<string>).data) as unknown;
        updateRef.current(mergeEndpointRuntime(endpoint, raw));
      } catch {
        // A malformed event is ignored; the periodic API projection remains authoritative.
      }
    });
    source.addEventListener("deleted", () => deletedRef.current());
    return () => source.close();
  }, [endpoint?.id, endpoint?.deletedAt, endpoint?.runtime?.uid]);
}

export function usePhaseHistory(endpoint: Endpoint): PhaseObservation[] {
  const storageKey = `ember.phase-history.${endpoint.id}`;
  const [history, setHistory] = useStoredState<PhaseObservation[]>(storageKey, []);
  const phase: EndpointPhase = endpoint.runtime?.phase ?? "Creating";
  const reason = currentReason(endpoint);

  useEffect(() => {
    setHistory((current) => recordPhaseObservation(current, phase, reason));
  }, [phase, reason, setHistory]);
  return history;
}
