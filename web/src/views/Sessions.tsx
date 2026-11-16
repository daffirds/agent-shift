import { useEffect, useState } from "react";
import { api, type SessionRecord, type State } from "../api";
import { Badge, Button, Spinner, ago } from "../ui";

const tone: Record<SessionRecord["state"], "ok" | "warn" | "bad" | "accent" | undefined> = {
  running: "accent",
  idle: "ok",
  queued: "warn",
  sleeping: undefined,
  failed: "bad",
  ended: undefined,
};

export function Sessions({ state }: { state: State }) {
  const [list, setList] = useState<SessionRecord[] | null>(null);
  const [err, setErr] = useState("");
  const [all, setAll] = useState(false);

  const load = () =>
    api.sessions().then(
      (l) => {
        setList(l);
        setErr("");
      },
      (e) => setErr((e as Error).message),
    );

  useEffect(() => {
    load();
    const t = setInterval(load, 5000);
    return () => clearInterval(t);
  }, []);

  const shown = (list ?? [])
    .filter((s) => all || s.state !== "ended")
    .sort((a, b) => new Date(b.last_active).getTime() - new Date(a.last_active).getTime());

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Sessions</h1>
          <p>One per Slack thread. Stop or interrupt them with <code>agent-shift sessions</code>, or by asking in the thread.</p>
        </div>
        <Button size="sm" kind="ghost" onClick={() => setAll(!all)}>
          {all ? "Hide ended" : "Show ended"}
        </Button>
      </div>
      {!state.daemon.running && <div className="notice warn">The daemon isn't running, so there are no live sessions to show.</div>}
      {err && <div className="notice bad">{err}</div>}
      <section className="card">
        {list === null && state.daemon.running ? (
          <div className="empty inline" style={{ justifyContent: "center" }}>
            <Spinner /> Loading…
          </div>
        ) : shown.length === 0 ? (
          <div className="empty">No {all ? "" : "active "}sessions.</div>
        ) : (
          <div className="rows">
            {shown.map((s) => (
              <div className="row" key={s.id}>
                <div className="row-main">
                  <div className="row-title">
                    <span className="mono faint">{s.id}</span>
                    {s.title || "(untitled)"}
                  </div>
                  <div className="row-sub">
                    {s.agent} · {s.channel_name ? "#" + s.channel_name.replace(/^#/, "") : s.channel} · {s.turns} turn{s.turns === 1 ? "" : "s"} · {ago(s.last_active)}
                  </div>
                  {s.last_error && <div className="hint bad">{s.last_error}</div>}
                </div>
                <div className="row-actions">
                  <Badge tone={tone[s.state]} dot pulse={s.state === "running"}>
                    {s.state}
                  </Badge>
                  {s.permalink && (
                    <a className="btn sm ghost" href={s.permalink} target="_blank" rel="noreferrer">
                      Open thread
                    </a>
                  )}
                </div>
              </div>
            ))}
          </div>
        )}
      </section>
    </>
  );
}
