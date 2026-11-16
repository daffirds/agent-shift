import { useEffect, useMemo, useState } from "react";
import { api, type Agent, type Conversation, type Model, type Ref, type Sender, type State } from "../api";
import { Badge, Button, Card, Chips, Field, Options, Picker, Seg, Spinner, Switch, useToast } from "../ui";

const ME = "me";

export function Agents({ state, refresh, onNeedConnect }: { state: State; refresh: () => Promise<void>; onNeedConnect: () => void }) {
  const toast = useToast();
  const [editing, setEditing] = useState<Agent | "new" | null>(null);

  if (editing) {
    return (
      <Editor
        state={state}
        initial={editing === "new" ? null : editing}
        onClose={async (saved) => {
          setEditing(null);
          if (saved) await refresh();
        }}
      />
    );
  }

  const toggle = async (a: Agent, on: boolean) => {
    try {
      const r = await api.saveAgent({ ...a, disabled: !on }, a.name);
      toast(`${a.name} ${on ? "enabled" : "disabled"}${r.reloaded ? "" : " (applies when the daemon starts)"}`);
      await refresh();
    } catch (e) {
      toast((e as Error).message, true);
    }
  };

  const remove = async (a: Agent) => {
    if (!confirm(`Delete agent “${a.name}”? Its sessions keep their history.`)) return;
    try {
      await api.deleteAgent(a.name);
      toast(`Deleted ${a.name}`);
      await refresh();
    } catch (e) {
      toast((e as Error).message, true);
    }
  };

  const ready = state.workspaces.length > 0;
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Agents</h1>
          <p>A trigger, a prompt, an engine and a directory. A matching top-level message starts a session; its thread steers it.</p>
        </div>
        <Button kind="primary" onClick={() => (ready ? setEditing("new") : onNeedConnect())}>
          New agent
        </Button>
      </div>
      {state.warnings.map((w) => (
        <div className="notice warn" key={w}>
          {w}
        </div>
      ))}
      <section className="card">
        {state.agents.length === 0 ? (
          <div className="empty">
            {ready ? "No agents yet." : "Connect a Slack workspace first, then create your first agent."}
            <div style={{ marginTop: 12 }}>
              <Button kind={ready ? "primary" : undefined} onClick={() => (ready ? setEditing("new") : onNeedConnect())}>
                {ready ? "Create an agent" : "Go to Connect"}
              </Button>
            </div>
          </div>
        ) : (
          <div className="rows">
            {state.agents.map((a) => {
              const ws = state.workspaces.find((w) => w.team_id === a.trigger.workspace);
              const convs = a.trigger.conversations.map((c) => c.name || c.id).join(", ");
              return (
                <div className="row" key={a.name}>
                  <Switch on={!a.disabled} onChange={(v) => toggle(a, v)} label={`Enable ${a.name}`} />
                  <div className="row-main">
                    <div className="row-title">
                      {a.name}
                      <Badge tone="accent">{a.engine === "codex" ? "Codex" : "Claude"}</Badge>
                      {a.model && <Badge>{a.model}</Badge>}
                      <Badge tone={a.autonomy === "full" ? "warn" : undefined}>{a.autonomy ?? "auto"}</Badge>
                    </div>
                    <div className="row-sub">
                      {ws ? ws.name + " · " : ""}
                      {convs || "no conversations"} → <span className="mono">{a.workdir}</span>
                    </div>
                  </div>
                  <div className="row-actions">
                    <Button size="sm" onClick={() => setEditing(a)}>
                      Edit
                    </Button>
                    <Button size="sm" kind="ghost" className="danger" onClick={() => remove(a)}>
                      Delete
                    </Button>
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </section>
    </>
  );
}

type Who = "anyone" | "me" | "pick";
type Steer = "anyone" | "me" | "trigger";

function whoOf(from?: Ref[]): Who {
  if (!from || from.length === 0) return "anyone";
  if (from.length === 1 && from[0].id === ME) return "me";
  return "pick";
}

function steerOf(tf?: Ref[]): Steer {
  if (!tf || tf.length === 0) return "anyone";
  if (tf.length === 1 && tf[0].id === ME) return "me";
  return "trigger";
}

function Editor({ state, initial, onClose }: { state: State; initial: Agent | null; onClose: (saved: boolean) => void }) {
  const toast = useToast();
  const firstWs = state.workspaces[0]?.team_id ?? "";
  const [a, setA] = useState<Agent>(
    () =>
      initial ?? {
        name: "",
        engine: state.engines.find((e) => e.id === "claude")?.installed || !state.engines.find((e) => e.id === "codex")?.installed ? "claude" : "codex",
        workdir: state.home,
        autonomy: "auto",
        trigger: { workspace: firstWs, conversations: [] },
      },
  );
  const set = (patch: Partial<Agent>) => setA((x) => ({ ...x, ...patch }));
  const setTrigger = (patch: Partial<Agent["trigger"]>) => setA((x) => ({ ...x, trigger: { ...x.trigger, ...patch } }));

  const [who, setWho] = useState<Who>(whoOf(initial?.trigger.from));
  const [steer, setSteer] = useState<Steer>(steerOf(initial?.thread_from));
  const [extraIds, setExtraIds] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState("");

  // --- workdir check ---
  const [dir, setDir] = useState<{ path: string; exists: boolean; git: boolean } | null>(null);
  useEffect(() => {
    const t = setTimeout(() => {
      api.checkDir(a.workdir).then(setDir, () => setDir(null));
    }, 300);
    return () => clearTimeout(t);
  }, [a.workdir]);

  // --- conversations ---
  const [convs, setConvs] = useState<Conversation[] | null>(null);
  const [convErr, setConvErr] = useState("");
  useEffect(() => {
    if (!a.trigger.workspace) return;
    setConvs(null);
    setConvErr("");
    api.conversations(a.trigger.workspace).then(setConvs, (e) => {
      setConvs([]);
      setConvErr((e as Error).message);
    });
  }, [a.trigger.workspace]);
  // Keep configured conversations visible even if Slack didn't list them.
  const convItems = useMemo(() => {
    const list = [...(convs ?? [])];
    for (const r of a.trigger.conversations) {
      if (!list.some((c) => c.id === r.id)) list.push({ id: r.id, label: r.name || r.id, name: r.name || r.id, kind: "channel" });
    }
    return list;
  }, [convs, a.trigger.conversations]);
  const selectedConvIds = a.trigger.conversations.map((c) => c.id);

  // --- senders ---
  const [senders, setSenders] = useState<Sender[] | null>(null);
  const convKey = selectedConvIds.join(",");
  useEffect(() => {
    if (who !== "pick" || !a.trigger.workspace || selectedConvIds.length === 0) {
      setSenders(null);
      return;
    }
    setSenders(null);
    api.senders(a.trigger.workspace, selectedConvIds).then(setSenders, () => setSenders([]));
  }, [who, convKey, a.trigger.workspace]);
  const me = state.workspaces.find((w) => w.team_id === a.trigger.workspace);
  const senderItems = useMemo(() => {
    const list: Sender[] = [{ id: ME, name: `${me?.user_name || "Me"} (me)`, kind: "person" }, ...(senders ?? [])];
    for (const r of a.trigger.from ?? []) {
      if (!list.some((s) => s.id === r.id)) list.push({ id: r.id, name: r.name || r.id, kind: r.id.startsWith("B") ? "workflow" : r.id.startsWith("A") ? "app" : "person" });
    }
    return list;
  }, [senders, a.trigger.from, me]);
  const selectedFrom = (a.trigger.from ?? []).map((r) => r.id);

  // --- models ---
  const [models, setModels] = useState<{ list: Model[]; error: string } | null>(null);
  const engineInfo = state.engines.find((e) => e.id === a.engine);
  useEffect(() => {
    setModels(null);
    if (!engineInfo?.installed) {
      setModels({ list: [], error: "" });
      return;
    }
    api.models(a.engine).then(
      (r) => setModels({ list: r.models, error: r.error }),
      (e) => setModels({ list: [], error: (e as Error).message }),
    );
  }, [a.engine]);
  const model = models?.list.find((m) => m.id === (a.model ?? ""));
  const customModel = !!a.model && models !== null && models.list.length > 0 && !model;
  const efforts = model?.efforts ?? (customModel || models?.list.length === 0 ? uniqueEfforts(models?.list ?? []) : []);

  const save = async () => {
    setErr("");
    if (!a.name.trim()) return setErr("Give the agent a name.");
    if (!a.trigger.workspace) return setErr("Pick a workspace.");
    if (a.trigger.conversations.length === 0) return setErr("Pick at least one conversation.");
    const from: Ref[] =
      who === "anyone"
        ? []
        : who === "me"
          ? [{ id: ME, name: me?.user_name }]
          : [
              ...selectedFrom.map((id) => ({ id, name: senderItems.find((s) => s.id === id)?.name.replace(/ \(me\)$/, "") })),
              ...extraIds.filter((id) => !selectedFrom.includes(id)).map((id) => ({ id })),
            ];
    if (who === "pick" && from.length === 0) return setErr("Pick who can trigger it, or choose Anyone.");
    const thread_from: Ref[] = steer === "anyone" ? [] : steer === "me" ? [{ id: ME }] : [{ id: ME }, ...from.filter((r) => r.id !== ME)];
    const out: Agent = { ...a, trigger: { ...a.trigger, from }, thread_from, worktree: dir?.git ? a.worktree : false };
    setSaving(true);
    try {
      const r = await api.saveAgent(out, initial?.name ?? "");
      toast(`Saved ${out.name}${r.reloaded ? " — daemon updated" : ""}`);
      r.warnings?.forEach((w) => toast(w, true));
      onClose(true);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const kindLabel: Record<Conversation["kind"], string> = { self: "notes", channel: "channel", private: "private", dm: "DM", group: "group DM" };
  const prefix: Record<Conversation["kind"], string> = { self: "✎ ", channel: "# ", private: "🔒 ", dm: "@ ", group: "⧉ " };

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{initial ? `Edit ${initial.name}` : "New agent"}</h1>
          <p>Saved to config.yaml and applied to the running daemon right away.</p>
        </div>
        <Button kind="ghost" onClick={() => onClose(false)}>
          ← Back
        </Button>
      </div>

      <Card title="Basics">
        <div className="card-body">
          <div className="grid2">
            <Field label="Name" hint="Short id, e.g. ticket-triage" htmlFor="name">
              <input id="name" value={a.name} onChange={(e) => set({ name: e.target.value })} placeholder="ticket-triage" autoFocus={!initial} />
            </Field>
            <Field label="Engine">
              <Seg
                value={a.engine}
                onChange={(v) => set({ engine: v, model: "", effort: "" })}
                options={state.engines.map((e) => ({ value: e.id as Agent["engine"], label: e.label + (e.installed ? "" : " (not installed)"), disabled: !e.installed }))}
              />
            </Field>
          </div>
          <Field
            label="Working directory"
            htmlFor="workdir"
            hint={
              dir === null ? (
                "Where the CLI session runs, usually a repo."
              ) : dir.exists ? (
                <span className="hint ok">{dir.git ? "Git repository" : "Folder exists (not a git repo)"}</span>
              ) : (
                <span className="hint bad">This folder doesn't exist</span>
              )
            }
          >
            <input id="workdir" className="mono" value={a.workdir} onChange={(e) => set({ workdir: e.target.value })} placeholder="~/code/my-repo" spellCheck={false} />
          </Field>
          {dir?.git && (
            <div className="toggle-row">
              <Switch on={!!a.worktree} onChange={(v) => set({ worktree: v })} label="Worktree per session" />
              <div>
                <div className="label">Give each session its own git worktree</div>
                <div className="hint">Several sessions can work on the repo at once without clashing.</div>
              </div>
            </div>
          )}
        </div>
      </Card>

      <Card title="Trigger" sub="Only top-level messages start a session. Replies in its thread steer it.">
        <div className="card-body">
          {state.workspaces.length > 1 && (
            <Field label="Workspace">
              <select value={a.trigger.workspace} onChange={(e) => setTrigger({ workspace: e.target.value, conversations: [] })}>
                {state.workspaces.map((w) => (
                  <option key={w.team_id} value={w.team_id}>
                    {w.name}
                  </option>
                ))}
              </select>
            </Field>
          )}
          <Field label="Conversations" hint={convErr ? <span className="hint bad">{convErr}</span> : `${selectedConvIds.length} selected. Only conversations you're a member of.`}>
            <Picker
              items={convItems}
              loading={convs === null}
              selected={selectedConvIds}
              onChange={(ids) => setTrigger({ conversations: ids.map((id) => ({ id, name: convItems.find((c) => c.id === id)?.name })) })}
              render={(c) => (
                <>
                  <span className="faint">{prefix[c.kind]}</span>
                  {c.label}
                </>
              )}
              kind={(c) => kindLabel[c.kind]}
              search={(c) => c.label + " " + c.id}
              placeholder="Search channels and DMs…"
            />
          </Field>
          <Field label="Who can trigger it">
            <Seg
              value={who}
              onChange={setWho}
              options={[
                { value: "anyone", label: "Anyone" },
                { value: "me", label: "Only me" },
                { value: "pick", label: "Specific senders" },
              ]}
            />
          </Field>
          {who === "pick" && (
            <>
              <Picker
                items={senderItems}
                loading={senders === null && selectedConvIds.length > 0}
                selected={selectedFrom}
                onChange={(ids) => setTrigger({ from: ids.map((id) => ({ id, name: senderItems.find((s) => s.id === id)?.name })) })}
                render={(s) => s.name}
                kind={(s) => (s.id === ME ? "you" : `${s.kind} · ${s.id}`)}
                search={(s) => s.name + " " + s.id}
                placeholder="Search recent senders…"
                empty="Pick conversations first. Recent senders in them show up here."
              />
              <Field label="Other IDs" hint="User (U…), bot or workflow (B…) or app (A…) IDs. Press Enter after each.">
                <Chips value={extraIds} onChange={setExtraIds} placeholder="B0123ABCD" />
              </Field>
            </>
          )}
          <div className="grid2">
            <Field label="Only messages containing" hint="Any of these, case-insensitive. Empty means every message.">
              <Chips value={a.trigger.contains ?? []} onChange={(v) => setTrigger({ contains: v })} placeholder="New ticket, [bug]" />
            </Field>
            <Field label="…and matching this regex" hint="Optional, Go RE2 syntax.">
              <input className="mono" value={a.trigger.regex ?? ""} onChange={(e) => setTrigger({ regex: e.target.value })} placeholder="(?i)priority:\s*(high|urgent)" spellCheck={false} />
            </Field>
          </div>
        </div>
      </Card>

      <Card title="Behaviour">
        <div className="card-body">
          <Field label="System prompt" hint="What should the agent do with these messages? Added on top of the engine's own prompt.">
            <textarea
              value={a.system_prompt ?? ""}
              onChange={(e) => set({ system_prompt: e.target.value })}
              placeholder="Each message is a ticket. Reproduce it, fix it on a new branch, open a draft PR with `gh pr create --draft`, and reply in the thread with the link."
              rows={7}
            />
          </Field>
          <Field label="Autonomy" hint="Nobody is at the keyboard: below Full, anything that would ask for permission is denied and the agent is told why.">
            <Options
              value={a.autonomy ?? "auto"}
              onChange={(v) => set({ autonomy: v })}
              options={[
                { value: "auto", label: "Auto", desc: "The engine's safety reviewer approves actions. Recommended." },
                { value: "full", label: "Full", desc: "No approvals, no sandbox. For unattended PRs and deploys." },
                { value: "readonly", label: "Read-only", desc: "Reads and answers, never changes anything." },
              ]}
            />
          </Field>
          <Field label="Who can steer it from the thread">
            <Seg
              value={steer}
              onChange={setSteer}
              options={[
                { value: "anyone", label: "Anyone in the thread" },
                { value: "me", label: "Only me" },
                { value: "trigger", label: "Me + trigger senders" },
              ]}
            />
          </Field>
        </div>
      </Card>

      <Card title="Model" sub={engineInfo?.installed ? `As offered by your ${engineInfo.label} login.` : undefined}>
        <div className="card-body">
          {models === null ? (
            <div className="inline muted">
              <Spinner /> Asking {engineInfo?.label ?? "the CLI"} which models you can use…
            </div>
          ) : (
            <div className="grid2">
              <Field label="Model" hint={models.error ? <span className="hint bad">Couldn't list models: {models.error}</span> : undefined}>
                {models.list.length > 0 ? (
                  <select
                    value={customModel ? "\u0000other" : (a.model ?? "")}
                    onChange={(e) => {
                      const v = e.target.value;
                      set({ model: v === "\u0000other" ? " " : v, effort: "" });
                    }}
                  >
                    <option value="">CLI default</option>
                    {models.list
                      .filter((m) => m.id !== "")
                      .map((m) => (
                        <option key={m.id} value={m.id}>
                          {m.name}
                          {m.description ? ` — ${m.description}` : ""}
                        </option>
                      ))}
                    <option value={"\u0000other"}>Other…</option>
                  </select>
                ) : null}
                {(customModel || models.list.length === 0) && (
                  <input className="mono" value={(a.model ?? "").trim()} onChange={(e) => set({ model: e.target.value })} placeholder="Leave empty for the CLI default" spellCheck={false} />
                )}
              </Field>
              <Field label="Reasoning effort" hint="Higher levels think longer, which is slower and uses more of your plan.">
                {efforts.length > 0 ? (
                  <select value={a.effort ?? ""} onChange={(e) => set({ effort: e.target.value })}>
                    <option value="">Default{model?.default_effort ? ` (${model.default_effort})` : ""}</option>
                    {efforts.map((e) => (
                      <option key={e.level} value={e.level}>
                        {e.level}
                        {e.description ? ` — ${e.description}` : ""}
                      </option>
                    ))}
                  </select>
                ) : model ? (
                  <input disabled value="Not adjustable for this model" />
                ) : (
                  <input value={a.effort ?? ""} onChange={(e) => set({ effort: e.target.value })} placeholder="e.g. low, medium, high" />
                )}
              </Field>
            </div>
          )}
          <details>
            <summary className="label" style={{ cursor: "pointer" }}>
              Advanced
            </summary>
            <div className="grid2" style={{ marginTop: 10 }}>
              <Field label="Message footer" hint={`Added to every Slack message it sends. Default: “Assisted by ${a.engine === "codex" ? "Codex" : "Claude"}”.`}>
                <input value={a.footer ?? ""} onChange={(e) => set({ footer: e.target.value })} placeholder={`Assisted by ${a.engine === "codex" ? "Codex" : "Claude"}`} />
              </Field>
              <Field label="Extra CLI arguments" hint={a.engine === "codex" ? "key=value config overrides. Enter after each." : "Extra claude flags. Enter after each."}>
                <Chips value={a.extra_args ?? []} onChange={(v) => set({ extra_args: v })} placeholder={a.engine === "codex" ? "model_verbosity=low" : "--add-dir=/tmp"} />
              </Field>
            </div>
          </details>
        </div>
      </Card>

      {err && <div className="notice bad">{err}</div>}
      <div className="inline" style={{ justifyContent: "flex-end" }}>
        <Button kind="ghost" onClick={() => onClose(false)}>
          Cancel
        </Button>
        <Button kind="primary" busy={saving} onClick={save}>
          {initial ? "Save changes" : "Create agent"}
        </Button>
      </div>
    </>
  );
}

function uniqueEfforts(list: Model[]) {
  const out: Model["efforts"] = [];
  for (const m of list) for (const e of m.efforts) if (!out.some((x) => x.level === e.level)) out.push(e);
  return out;
}
