import { createContext, useContext, useMemo, useState, type ReactNode } from "react";

// ---- toasts ----

type Toast = { id: number; text: string; bad?: boolean };
const ToastCtx = createContext<(text: string, bad?: boolean) => void>(() => {});
export const useToast = () => useContext(ToastCtx);

export function Toasts({ children }: { children: ReactNode }) {
  const [list, setList] = useState<Toast[]>([]);
  const push = useMemo(
    () => (text: string, bad?: boolean) => {
      const id = Date.now() + Math.random();
      setList((l) => [...l, { id, text, bad }]);
      setTimeout(() => setList((l) => l.filter((t) => t.id !== id)), bad ? 7000 : 3200);
    },
    [],
  );
  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="toasts" role="status" aria-live="polite">
        {list.map((t) => (
          <div key={t.id} className={"toast" + (t.bad ? " bad" : "")}>
            {t.text}
          </div>
        ))}
      </div>
    </ToastCtx.Provider>
  );
}

// ---- primitives ----

export function Spinner() {
  return <span className="spin" aria-label="Loading" />;
}

export function Button({
  kind,
  size,
  busy,
  children,
  ...rest
}: React.ButtonHTMLAttributes<HTMLButtonElement> & { kind?: "primary" | "ghost" | "danger"; size?: "sm"; busy?: boolean }) {
  return (
    <button
      type="button"
      {...rest}
      disabled={rest.disabled || busy}
      className={["btn", kind, size, rest.className].filter(Boolean).join(" ")}
    >
      {busy && <Spinner />}
      {children}
    </button>
  );
}

export function Switch({ on, onChange, label }: { on: boolean; onChange: (v: boolean) => void; label: string }) {
  return <button type="button" role="switch" aria-checked={on} aria-label={label} className={"switch" + (on ? " on" : "")} onClick={() => onChange(!on)} />;
}

export function Badge({ tone, dot, pulse, children }: { tone?: "ok" | "warn" | "bad" | "accent"; dot?: boolean; pulse?: boolean; children: ReactNode }) {
  return (
    <span className={"badge" + (tone ? " " + tone : "")}>
      {dot && <span className={"dot" + (pulse ? " pulse" : "")} />}
      {children}
    </span>
  );
}

export function Card({ title, sub, action, children, foot, step }: { title: ReactNode; sub?: ReactNode; action?: ReactNode; children?: ReactNode; foot?: ReactNode; step?: { n: number; done: boolean } }) {
  return (
    <section className="card">
      <div className="card-head">
        <div>
          <h2>
            {step && <span className={"step" + (step.done ? " done" : "")}>{step.done ? "✓" : step.n}</span>}
            {title}
          </h2>
          {sub && <p>{sub}</p>}
        </div>
        {action}
      </div>
      {children}
      {foot && <div className="card-foot">{foot}</div>}
    </section>
  );
}

export function Field({ label, hint, children, htmlFor }: { label: ReactNode; hint?: ReactNode; children: ReactNode; htmlFor?: string }) {
  return (
    <div className="field">
      <label htmlFor={htmlFor}>{label}</label>
      {children}
      {hint && (typeof hint === "string" ? <span className="hint">{hint}</span> : hint)}
    </div>
  );
}

export function Seg<T extends string>({ value, options, onChange }: { value: T; options: { value: T; label: string; disabled?: boolean }[]; onChange: (v: T) => void }) {
  return (
    <div className="seg" role="radiogroup">
      {options.map((o) => (
        <button key={o.value} type="button" role="radio" aria-checked={value === o.value} disabled={o.disabled} className={value === o.value ? "on" : ""} onClick={() => onChange(o.value)}>
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function Options<T extends string>({ value, options, onChange }: { value: T; options: { value: T; label: string; desc: string }[]; onChange: (v: T) => void }) {
  return (
    <div className="options" role="radiogroup">
      {options.map((o) => (
        <button key={o.value} type="button" role="radio" aria-checked={value === o.value} className={"option" + (value === o.value ? " on" : "")} onClick={() => onChange(o.value)}>
          <b>{o.label}</b>
          <span>{o.desc}</span>
        </button>
      ))}
    </div>
  );
}

// Picker is a searchable multi-select list.
export function Picker<T extends { id: string }>({
  items,
  selected,
  onChange,
  render,
  kind,
  search,
  placeholder,
  loading,
  empty,
}: {
  items: T[];
  selected: string[];
  onChange: (ids: string[]) => void;
  render: (it: T) => ReactNode;
  kind?: (it: T) => string;
  search: (it: T) => string;
  placeholder: string;
  loading?: boolean;
  empty?: ReactNode;
}) {
  const [q, setQ] = useState("");
  const shown = items.filter((it) => !q || search(it).toLowerCase().includes(q.toLowerCase()));
  // Selected items first, so a long list doesn't hide what's chosen.
  shown.sort((a, b) => Number(selected.includes(b.id)) - Number(selected.includes(a.id)));
  const toggle = (id: string) => onChange(selected.includes(id) ? selected.filter((x) => x !== id) : [...selected, id]);
  return (
    <div className="picker">
      <input className="picker-search" placeholder={placeholder} value={q} onChange={(e) => setQ(e.target.value)} />
      <div className="picker-list">
        {loading && (
          <div className="picker-empty inline">
            <Spinner /> Loading…
          </div>
        )}
        {!loading && shown.length === 0 && <div className="picker-empty">{q ? "No matches." : (empty ?? "Nothing here yet.")}</div>}
        {!loading &&
          shown.map((it) => (
            <label key={it.id} className="pick">
              <input type="checkbox" checked={selected.includes(it.id)} onChange={() => toggle(it.id)} />
              <span>{render(it)}</span>
              {kind && <span className="kind">{kind(it)}</span>}
            </label>
          ))}
      </div>
    </div>
  );
}

// Chips edits a list of short strings.
export function Chips({ value, onChange, placeholder }: { value: string[]; onChange: (v: string[]) => void; placeholder: string }) {
  const [draft, setDraft] = useState("");
  const add = () => {
    const parts = draft
      .split(",")
      .map((s) => s.trim())
      .filter((s) => s && !value.includes(s));
    if (parts.length) onChange([...value, ...parts]);
    setDraft("");
  };
  return (
    <div className="chips">
      {value.map((v) => (
        <span className="chip" key={v}>
          {v}
          <button type="button" aria-label={`Remove ${v}`} onClick={() => onChange(value.filter((x) => x !== v))}>
            ×
          </button>
        </span>
      ))}
      <input
        value={draft}
        placeholder={value.length ? "" : placeholder}
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === ",") {
            e.preventDefault();
            add();
          } else if (e.key === "Backspace" && !draft && value.length) {
            onChange(value.slice(0, -1));
          }
        }}
        onBlur={add}
      />
    </div>
  );
}

export function ago(iso?: string): string {
  if (!iso) return "";
  const t = new Date(iso).getTime();
  if (!t || t < 0) return "";
  const s = Math.max(0, (Date.now() - t) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}
