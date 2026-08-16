import { useState } from "react";

import type { Endpoint } from "../types";
import { Icon } from "./Icon";
import { Button, InlineError } from "./Primitives";

interface DeleteDialogProps {
  endpoint: Endpoint;
  open: boolean;
  onClose: () => void;
  onDelete: (endpointID: string) => Promise<void>;
}

export function DeleteDialog({ endpoint, open, onClose, onDelete }: DeleteDialogProps) {
  const [deleting, setDeleting] = useState(false);
  const [error, setError] = useState("");

  const confirm = async () => {
    setDeleting(true);
    setError("");
    try {
      await onDelete(endpoint.id);
      onClose();
    } catch (deleteError) {
      setError(deleteError instanceof Error ? deleteError.message : "Deletion could not be requested");
    } finally {
      setDeleting(false);
    }
  };

  if (!open) {
    return null;
  }
  return (
    <div className="modal-backdrop">
      <section aria-modal="true" className="delete-dialog" role="dialog">
        <div className="delete-icon"><Icon name="trash" size={22} /></div>
        <span className="eyebrow">Finalizer-driven cleanup</span>
        <h2>Delete {endpoint.displayName}?</h2>
        <p>
          Ember will stop serving, wait for Pods to disappear, delete the dedicated namespace,
          then remove the CR finalizer.
        </p>
        <div className="delete-sequence">
          {[
            ["Scale Deployment", "0 replicas"],
            ["Wait for Pods", "no GPU allocation"],
            ["Delete Namespace", "children reclaimed"],
            ["Retain metadata", "audit remains readable"]
          ].map(([title, detail], index) => (
            <div key={title}>
              <span>{index + 1}</span>
              <div><strong>{title}</strong><small>{detail}</small></div>
              {index < 3 && <Icon name="arrow" size={14} />}
            </div>
          ))}
        </div>
        <div className="delete-retention-note">
          <Icon name="database" size={16} />
          This deletes runtime resources, not the Postgres presentation record or append-only audit.
        </div>
        {error && <InlineError message={error} />}
        <footer>
          <Button disabled={deleting} variant="ghost" onClick={onClose}>Cancel</Button>
          <Button disabled={deleting} icon="trash" variant="danger" onClick={() => void confirm()}>
            {deleting ? "Requesting cleanup…" : "Delete runtime"}
          </Button>
        </footer>
      </section>
    </div>
  );
}
