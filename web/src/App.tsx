import { useCallback, useEffect, useState } from "react";
import { api, ApiError, hasToken, type State } from "./api";
import { Badge, Button, Spinner, useToast } from "./ui";
import { Agents } from "./views/Agents";
import { Connect } from "./views/Connect";
import { Sessions } from "./views/Sessions";
import { Settings } from "./views/Settings";

type Tab = "connect" | "agents" | "sessions" | "settings";
const TABS: { id: Tab; label: string }[] = [
  { id: "connect", label: "Connect" },
  { id: "agents", label: "Agents" },
  { id: "sessions", label: "Sessions" },
  { id: "settings", label: "Settings" },
];

export function App() {
  if (!hasToken()) return <NeedLink />;
  return <Shell />;
}

function Shell() {
  const toast = useToast();
  const [state, setState] = useState<State | null>(null);
  const [fatal, setFatal] = useState("");
  const [tab, setTab] = useState<Tab | null>(null);
  const [busy, setBusy] = useState("");

  const refresh = useCallback(async () => {
    try {
      setState(await api.state());
      setFatal("");
    } catch (e) {
      setFatal(e instanceof ApiError && e.status === 401 ? "This link has expired. Run `agent-shift ui` again." : (e as Error).message);
    }
  }, []);

  useEffect(() => {
    refresh();
    const t = setInterval(refresh, 8000);
    return () => clearInterval(t);
  }, [refresh]);

  // First visit: land where the next setup step is.
  useEffect(() => {
    if (state && tab === null) setTab(state.workspaces.length > 0 ? "agents" : "connect");
  }, [state, tab]);

  const daemon = async (action: "start" | "stop" | "restart") => {
    setBusy(action);
    try {
      await api.daemon(action);
      toast(action === "stop" ? "Daemon stopped" : action === "start" ? "Daemon started" : "Daemon restarted");
      await refresh();
    } catch (e) {
      toast((e as Error).message, true);
    } finally {
      setBusy("");
    }
  };

  if (fatal && !state) return <Center title="Can't reach agent-shift" text={fatal} />;
  if (!state || tab === null)
    return (
      <div className="center">
        <Spinner />
      </div>
    );

  const sessions = state.daemon.status?.sessions ?? {};
  const live = (sessions.running ?? 0) + (sessions.idle ?? 0) + (sessions.queued ?? 0);

  return (
    <>
      <header className="top">
        <div className="top-inner">
          <div className="brand">
            <span className="brand-mark">
              <svg width="14" height="14" viewBox="0 0 32 32" aria-hidden>
                <path d="M7 21l9-10 9 10" stroke="white" strokeWidth="4" fill="none" strokeLinecap="round" strokeLinejoin="round" />
              </svg>
            </span>
            agent-shift
          </div>
          <nav className="tabs">
            {TABS.map((t) => (
              <button key={t.id} className={"tab" + (tab === t.id ? " on" : "")} onClick={() => setTab(t.id)}>
                {t.label}
              </button>
            ))}
          </nav>
          <div className="spacer" />
          <div className="daemon">
            {state.daemon.running ? (
              <>
                <Badge tone="ok" dot>
                  Running{live ? ` · ${live} live` : ""}
                </Badge>
                <Button size="sm" kind="ghost" busy={busy === "restart"} onClick={() => daemon("restart")}>
                  Restart
                </Button>
                <Button size="sm" busy={busy === "stop"} onClick={() => daemon("stop")}>
                  Stop
                </Button>
              </>
            ) : (
              <>
                <Badge dot>Stopped</Badge>
                <Button size="sm" kind="primary" busy={busy === "start"} onClick={() => daemon("start")}>
                  Start daemon
                </Button>
              </>
            )}
          </div>
        </div>
      </header>
      <main>
        <nav className="seg mobile-tabs" aria-label="Sections">
          {TABS.map((t) => (
            <button key={t.id} className={tab === t.id ? "on" : ""} onClick={() => setTab(t.id)}>
              {t.label}
            </button>
          ))}
        </nav>
        {fatal && <div className="notice bad">{fatal}</div>}
        {tab === "connect" && <Connect state={state} refresh={refresh} />}
        {tab === "agents" && <Agents state={state} refresh={refresh} onNeedConnect={() => setTab("connect")} />}
        {tab === "sessions" && <Sessions state={state} />}
        {tab === "settings" && <Settings state={state} refresh={refresh} />}
      </main>
    </>
  );
}

function Center({ title, text }: { title: string; text: string }) {
  return (
    <div className="center">
      <div className="card">
        <h2>{title}</h2>
        <p className="muted" style={{ marginTop: 6 }}>
          {text}
        </p>
      </div>
    </div>
  );
}

function NeedLink() {
  return <Center title="Open the link from your terminal" text="Run `agent-shift ui` and open the link it prints. It carries a one-time key that keeps other sites out." />;
}

