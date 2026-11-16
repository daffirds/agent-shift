import { useState } from "react";
import { api, type State, type Workspace, type WorkspaceStatus } from "../api";
import { Badge, Button, Card, useToast } from "../ui";

export function Connect({ state, refresh }: { state: State; refresh: () => Promise<void> }) {
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Connect</h1>
          <p>agent-shift reads Slack by polling with your own user token. Slack never pushes events, so there is nothing for it to switch off while this computer is off.</p>
        </div>
      </div>
      <AppCard state={state} />
      <WorkspacesCard state={state} refresh={refresh} />
      <EnginesCard state={state} />
    </>
  );
}

function AppCard({ state }: { state: State }) {
  const toast = useToast();
  const [show, setShow] = useState(false);
  const done = state.workspaces.length > 0;
  return (
    <Card
      step={{ n: 1, done }}
      title="Slack app"
      sub="A private app in your workspace that only holds your user token. No bot, no event subscription, no Socket Mode."
      action={
        <a className="btn primary sm" href={state.slack.create_app_url} target="_blank" rel="noreferrer">
          Create app in Slack
        </a>
      }
    >
      <div className="card-body">
        <ol className="steps">
          <li>
            <b>Create app in Slack</b> opens Slack with the manifest filled in. Pick your workspace and click <b>Create</b>.
          </li>
          <li>
            Click <b>Install to Workspace</b> and allow it.
          </li>
          <li>
            Under <b>OAuth &amp; Permissions</b>, copy the <b>User OAuth Token</b> (<code>xoxp-…</code>) and paste it below.
          </li>
        </ol>
        <div className="inline">
          <Button size="sm" kind="ghost" onClick={() => setShow(!show)}>
            {show ? "Hide manifest" : "Show manifest"}
          </Button>
          {show && (
            <Button
              size="sm"
              kind="ghost"
              onClick={() => navigator.clipboard.writeText(state.slack.manifest).then(() => toast("Manifest copied"))}
            >
              Copy
            </Button>
          )}
        </div>
        {show && <pre className="manifest">{state.slack.manifest}</pre>}
      </div>
    </Card>
  );
}

function WorkspacesCard({ state, refresh }: { state: State; refresh: () => Promise<void> }) {
  const toast = useToast();
  const [adding, setAdding] = useState(false);
  const status = new Map((state.daemon.status?.workspaces ?? []).map((w) => [w.team_id, w]));

  const remove = async (team: string, name: string) => {
    if (!confirm(`Stop using ${name}? Its token is deleted from your keychain.`)) return;
    try {
      await api.removeWorkspace(team);
      toast(`Removed ${name}`);
      await refresh();
    } catch (e) {
      toast((e as Error).message, true);
    }
  };

  return (
    <Card
      step={{ n: 2, done: state.workspaces.length > 0 }}
      title="Slack workspaces"
      sub="Agents read and post as you, with this token. It is stored in your OS keychain, never in the config file."
      action={
        !adding && state.workspaces.length > 0 ? (
          <Button size="sm" onClick={() => setAdding(true)}>
            Add workspace
          </Button>
        ) : undefined
      }
    >
      {state.workspaces.length > 0 && (
        <div className="rows" style={{ marginTop: 10 }}>
          {state.workspaces.map((w) => (
            <WorkspaceRow key={w.team_id} w={w} st={status.get(w.team_id)} running={state.daemon.running} onRemove={() => remove(w.team_id, w.name)} refresh={refresh} />
          ))}
        </div>
      )}
      {(adding || state.workspaces.length === 0) && (
        <div className="card-body">
          <TokenForm
            label="Connect"
            refresh={refresh}
            onDone={() => setAdding(false)}
            onCancel={state.workspaces.length > 0 ? () => setAdding(false) : undefined}
          />
        </div>
      )}
    </Card>
  );
}

function WorkspaceRow({ w, st, running, onRemove, refresh }: { w: Workspace; st?: WorkspaceStatus; running: boolean; onRemove: () => void; refresh: () => Promise<void> }) {
  const [replacing, setReplacing] = useState(false);
  const p = st?.poll;
  const behind = p?.covered ? Date.now() - new Date(p.covered).getTime() : 0;
  return (
    <div className="row" style={{ flexWrap: "wrap" }}>
      <div className="row-main">
        <div className="row-title">
          {w.name}
          {w.token_error ? (
            <Badge tone="bad">no token</Badge>
          ) : !running ? (
            <Badge>daemon stopped</Badge>
          ) : st?.connected ? (
            behind > 3 * 60_000 ? (
              <Badge tone="warn" dot pulse>
                catching up
              </Badge>
            ) : (
              <Badge tone="ok" dot>
                polling
              </Badge>
            )
          ) : (
            <Badge tone="warn" dot pulse>
              {st?.error ? "problem" : "starting"}
            </Badge>
          )}
        </div>
        <div className="row-sub">
          as {w.user_name || w.user_id}
          {w.token_hint && (
            <>
              {" · "}
              <span className="mono">{w.token_hint}</span>
            </>
          )}
          {p && running && (
            <>
              {" · "}
              {p.conversations} {p.conversations === 1 ? "conversation" : "conversations"}, {p.threads} {p.threads === 1 ? "thread" : "threads"} ({p.hot} active)
            </>
          )}
        </div>
        {(w.token_error || st?.error) && <div className="hint bad">{w.token_error || st?.error}</div>}
        {p?.search && !["on", "idle"].includes(p.search) && <div className="hint">Search {p.search}</div>}
      </div>
      <div className="row-actions">
        <Button size="sm" kind="ghost" onClick={() => setReplacing(!replacing)}>
          {replacing ? "Cancel" : "Replace token"}
        </Button>
        <Button size="sm" kind="ghost" className="danger" onClick={onRemove}>
          Remove
        </Button>
      </div>
      {replacing && (
        <div style={{ flexBasis: "100%" }}>
          <TokenForm label="Replace" refresh={refresh} onDone={() => setReplacing(false)} />
        </div>
      )}
    </div>
  );
}

function TokenForm({ label, refresh, onDone, onCancel }: { label: string; refresh: () => Promise<void>; onDone: () => void; onCancel?: () => void }) {
  const toast = useToast();
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const submit = async () => {
    setBusy(true);
    setErr("");
    try {
      const r = await api.addWorkspace(token.trim());
      toast(`Connected ${r.workspace.name} as ${r.workspace.user_name || r.workspace.user_id}${r.reloaded ? " — daemon updated" : ""}`);
      if (r.missing_scopes?.length) toast(`The token lacks Slack scopes: ${r.missing_scopes.join(", ")}. Add them to the app and reinstall it.`, true);
      setToken("");
      await refresh();
      onDone();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <form
        className="inline"
        onSubmit={(e) => {
          e.preventDefault();
          if (token.trim()) submit();
        }}
      >
        <input
          type="password"
          autoComplete="off"
          spellCheck={false}
          placeholder="xoxp-…  User OAuth Token"
          value={token}
          onChange={(e) => setToken(e.target.value)}
          aria-label="Slack User OAuth Token"
          autoFocus
        />
        <Button kind="primary" busy={busy} disabled={!token.trim()} type="submit">
          {label}
        </Button>
        {onCancel && (
          <Button kind="ghost" onClick={onCancel}>
            Cancel
          </Button>
        )}
      </form>
      {err && <span className="hint bad">{err}</span>}
    </>
  );
}

function EnginesCard({ state }: { state: State }) {
  return (
    <Card title="Coding agents" sub="The CLIs run with their own logins, so work counts against your Claude or ChatGPT plan.">
      <div className="rows" style={{ marginTop: 10 }}>
        {state.engines.map((e) => (
          <div className="row" key={e.id}>
            <div className="row-main">
              <div className="row-title">
                {e.label}
                {!e.installed ? <Badge>not installed</Badge> : e.logged_in ? <Badge tone="ok" dot>ready</Badge> : <Badge tone="warn">not logged in</Badge>}
              </div>
              <div className="row-sub">
                {!e.installed ? (
                  <a href={e.install} target="_blank" rel="noreferrer">
                    Install {e.label}
                  </a>
                ) : e.logged_in ? (
                  [e.version, e.detail].filter(Boolean).join(" · ")
                ) : (
                  <>
                    Run <code>{e.login}</code> in a terminal
                  </>
                )}
              </div>
            </div>
          </div>
        ))}
      </div>
    </Card>
  );
}
