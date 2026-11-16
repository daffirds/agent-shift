import { useState } from "react";
import { api, type Settings as S, type State } from "../api";
import { Button, Card, Field, Switch, useToast } from "../ui";

export function Settings({ state, refresh }: { state: State; refresh: () => Promise<void> }) {
  const toast = useToast();
  const [s, setS] = useState<S>(state.settings);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [asBusy, setAsBusy] = useState(false);
  const dirty = JSON.stringify(s) !== JSON.stringify(state.settings);
  const set = (p: Partial<S>) => setS((x) => ({ ...x, ...p }));

  const save = async () => {
    setBusy(true);
    setErr("");
    try {
      const r = await api.saveSettings(s);
      toast(r.reloaded ? "Saved — daemon updated" : "Saved");
      await refresh();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const autostart = async (on: boolean) => {
    setAsBusy(true);
    try {
      await api.autostart(on);
      toast(on ? "agent-shift will start when you log in" : "Autostart disabled");
      await refresh();
    } catch (e) {
      toast((e as Error).message, true);
    } finally {
      setAsBusy(false);
    }
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Settings</h1>
          <p>How the daemon runs sessions. Durations look like 30m, 12h or 7d.</p>
        </div>
      </div>
      <Card
        title="Daemon"
        foot={
          <>
            {dirty && (
              <Button kind="ghost" onClick={() => setS(state.settings)}>
                Reset
              </Button>
            )}
            <Button kind="primary" busy={busy} disabled={!dirty} onClick={save}>
              Save
            </Button>
          </>
        }
      >
        <div className="card-body">
          <div className="grid2">
            <Field label="Max sessions" hint="CLI processes running at once. Extra work waits in a queue.">
              <input type="number" min={1} max={50} value={s.max_sessions} onChange={(e) => set({ max_sessions: Number(e.target.value) })} />
            </Field>
            <Field label="Idle timeout" hint="Stop a session's CLI after this long without activity. The next reply resumes it.">
              <input value={s.idle_timeout} onChange={(e) => set({ idle_timeout: e.target.value })} />
            </Field>
            <Field label="Thread TTL" hint="How long a quiet thread keeps steering its session.">
              <input value={s.thread_ttl} onChange={(e) => set({ thread_ttl: e.target.value })} />
            </Field>
            <Field label="Catch-up window" hint={`After downtime, replay missed Slack messages up to this far back. "off" disables it.`}>
              <input value={s.catch_up} onChange={(e) => set({ catch_up: e.target.value })} />
            </Field>
            <Field label="Log level">
              <select value={s.log_level} onChange={(e) => set({ log_level: e.target.value })}>
                {["debug", "info", "warn", "error"].map((l) => (
                  <option key={l}>{l}</option>
                ))}
              </select>
            </Field>
          </div>
          {err && <div className="notice bad">{err}</div>}
        </div>
      </Card>

      {state.autostart.supported && (
        <Card title="Start at login">
          <div className="card-body">
            <div className="toggle-row">
              <Switch on={state.autostart.enabled} onChange={(v) => !asBusy && autostart(v)} label="Start at login" />
              <div>
                <div className="label">Start agent-shift automatically when you log in</div>
                <div className="hint">{state.autostart.enabled ? state.autostart.detail : "A LaunchAgent on macOS, a per-user Run entry on Windows."}</div>
              </div>
            </div>
          </div>
        </Card>
      )}

      <Card title="Files">
        <div className="card-body">
          <dl className="kv">
            <dt>Config</dt>
            <dd className="mono">{state.paths.config}</dd>
            <dt>Daemon log</dt>
            <dd className="mono">{state.paths.log}</dd>
            <dt>Version</dt>
            <dd className="mono">{state.version}</dd>
          </dl>
        </div>
      </Card>
    </>
  );
}
