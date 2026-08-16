import type { ButtonHTMLAttributes, HTMLAttributes, ReactNode } from "react";

import type { EndpointPhase } from "../types";
import { phaseTone } from "../utils";
import { Icon } from "./Icon";

interface PanelProps extends HTMLAttributes<HTMLElement> {
  title?: string;
  eyebrow?: string;
  icon?: Parameters<typeof Icon>[0]["name"];
  action?: ReactNode;
  children: ReactNode;
}

export function Panel({ title, eyebrow, icon, action, children, className = "", ...props }: PanelProps) {
  return (
    <section className={`panel ${className}`.trim()} {...props}>
      {(title || eyebrow || action) && (
        <header className="panel-header">
          <div>
            {eyebrow && <span className="eyebrow">{eyebrow}</span>}
            {title && (
              <h2 className="panel-title">
                {icon && <Icon name={icon} size={17} />}
                {title}
              </h2>
            )}
          </div>
          {action}
        </header>
      )}
      {children}
    </section>
  );
}

export function PhaseBadge({ phase }: { phase?: EndpointPhase }) {
  const resolved = phase ?? "Creating";
  return (
    <span className={`phase-badge tone-${phaseTone(resolved)}`}>
      <span className="status-dot" />
      {resolved}
    </span>
  );
}

export function Tag({ children, tone = "neutral" }: { children: ReactNode; tone?: string }) {
  return <span className={`tag tag-${tone}`}>{children}</span>;
}

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  icon?: Parameters<typeof Icon>[0]["name"];
  variant?: "primary" | "secondary" | "ghost" | "danger";
}

export function Button({ icon, variant = "secondary", children, className = "", ...props }: ButtonProps) {
  return (
    <button className={`button button-${variant} ${className}`.trim()} {...props}>
      {icon && <Icon name={icon} size={16} />}
      {children}
    </button>
  );
}

export function IconButton({
  label,
  icon,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  label: string;
  icon: Parameters<typeof Icon>[0]["name"];
}) {
  return (
    <button aria-label={label} className="icon-button" title={label} {...props}>
      <Icon name={icon} size={17} />
    </button>
  );
}

export function Spinner({ label = "Loading" }: { label?: string }) {
  return (
    <span className="spinner-wrap">
      <span className="spinner" />
      <span>{label}</span>
    </span>
  );
}

export function InlineError({ message }: { message: string }) {
  return (
    <div className="inline-error">
      <span>!</span>
      {message}
    </div>
  );
}

export function Skeleton({ className = "" }: { className?: string }) {
  return <span className={`skeleton ${className}`.trim()} />;
}
