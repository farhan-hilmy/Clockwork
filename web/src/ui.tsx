import { useEffect, useRef, type ButtonHTMLAttributes, type ReactNode } from "react";

type Variant = "primary" | "secondary" | "ghost" | "danger";

const variants: Record<Variant, string> = {
  primary: "bg-brass text-on-accent hover:brightness-110",
  secondary: "bg-raised text-fg border border-line hover:bg-hover",
  ghost: "text-fg-2 hover:text-fg hover:bg-raised",
  danger: "bg-danger/15 text-danger border border-danger/40 hover:bg-danger/25",
};

export function Button({
  variant = "secondary",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant }) {
  return (
    <button
      {...props}
      className={`inline-flex items-center gap-1.5 rounded-md px-3.5 py-2 text-[13px] font-medium transition disabled:cursor-not-allowed disabled:opacity-50 ${variants[variant]} ${className}`}
    />
  );
}

type Tone = "ok" | "danger" | "info" | "warn" | "neutral";
const tones: Record<Tone, string> = {
  ok: "bg-ok/15 text-ok",
  danger: "bg-danger/15 text-danger",
  info: "bg-info/15 text-info",
  warn: "bg-warn/15 text-warn",
  neutral: "bg-neutral/15 text-neutral",
};
const dot: Record<Tone, string> = { ok: "bg-ok", danger: "bg-danger", info: "bg-info", warn: "bg-warn", neutral: "bg-neutral" };

export function Badge({ tone, children }: { tone: Tone; children: ReactNode }) {
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 font-mono text-[11px] font-medium ${tones[tone]}`}>
      <span className={`h-1.5 w-1.5 rounded-full ${dot[tone]}`} />
      {children}
    </span>
  );
}

export function Card({ title, actions, children, className = "" }: { title?: ReactNode; actions?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={`overflow-hidden rounded-[10px] border border-line bg-surface ${className}`}>
      {title && (
        <header className="flex items-center gap-2 px-4 py-3.5">
          <h2 className="flex-1 text-sm font-semibold">{title}</h2>
          {actions}
        </header>
      )}
      {children}
    </section>
  );
}

export function PageHeader({ title, subtitle, actions, crumb }: { title: ReactNode; subtitle?: ReactNode; actions?: ReactNode; crumb?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-end gap-4">
      <div className="min-w-0 flex-1">
        {crumb && <div className="mb-1.5 font-mono text-xs text-fg-3">{crumb}</div>}
        <h1 className="text-2xl font-semibold">{title}</h1>
        {subtitle && <p className="mt-1.5 text-sm text-fg-2">{subtitle}</p>}
      </div>
      {actions && <div className="flex gap-2">{actions}</div>}
    </div>
  );
}

export function Spinner({ label = "Loading…" }: { label?: string }) {
  return (
    <div role="status" className="flex items-center gap-3 px-4 py-10 text-sm text-fg-2">
      <span className="h-4 w-4 animate-spin rounded-full border-2 border-line-strong border-t-brass" />
      {label}
    </div>
  );
}

export function EmptyState({ title, body, action }: { title: string; body?: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex flex-col items-center gap-2 px-6 py-14 text-center">
      <p className="text-sm font-medium">{title}</p>
      {body && <p className="max-w-md text-sm text-fg-2">{body}</p>}
      {action && <div className="mt-3">{action}</div>}
    </div>
  );
}

export function ErrorState({ error, onRetry }: { error: Error; onRetry?: () => void }) {
  return (
    <div role="alert" className="flex flex-col items-center gap-2 px-6 py-12 text-center">
      <p className="text-sm font-medium text-danger">Something went wrong</p>
      <p className="max-w-md text-sm text-fg-2">{error.message}</p>
      {onRetry && (
        <Button className="mt-3" onClick={onRetry}>
          Try again
        </Button>
      )}
    </div>
  );
}

/** Modal confirmation for destructive actions. Uses the native <dialog> element. */
export function ConfirmDialog({
  open,
  title,
  body,
  confirmLabel,
  busy,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  title: string;
  body: ReactNode;
  confirmLabel: string;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) d.showModal?.();
    if (!open && d.open) d.close?.();
  }, [open]);
  if (!open) return null;
  return (
    <dialog
      ref={ref}
      onCancel={(e) => {
        e.preventDefault();
        onCancel();
      }}
      className="m-auto w-[420px] max-w-[calc(100vw-32px)] rounded-xl border border-line bg-surface p-0 text-fg backdrop:bg-black/60"
    >
      <div className="p-5">
        <h2 className="text-base font-semibold">{title}</h2>
        <div className="mt-2 text-sm text-fg-2">{body}</div>
      </div>
      <div className="flex justify-end gap-2 border-t border-line px-5 py-3">
        <Button variant="ghost" onClick={onCancel} disabled={busy}>
          Cancel
        </Button>
        <Button variant="danger" onClick={onConfirm} disabled={busy}>
          {busy ? "Working…" : confirmLabel}
        </Button>
      </div>
    </dialog>
  );
}

export function Field({ label, hint, error, children }: { label: string; hint?: ReactNode; error?: string; children: ReactNode }) {
  return (
    <label className="flex flex-col gap-1.5">
      <span className="text-xs font-medium text-fg-2">{label}</span>
      {children}
      {error ? <span className="text-xs text-danger">{error}</span> : hint ? <span className="text-xs text-fg-3">{hint}</span> : null}
    </label>
  );
}

export const inputClass =
  "w-full rounded-md border border-line bg-canvas px-3 py-2 text-[13px] text-fg placeholder:text-fg-3 focus:border-brass focus:outline-none aria-[invalid=true]:border-danger";
