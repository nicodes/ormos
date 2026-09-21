import { createEffect, createSignal, For, onCleanup, Show } from "solid-js";

type SystemInfo = {
  version: string;
  hostname: string;
  os: string;
  arch: string;
  shell: string;
  configDir: string;
  hasConfig: boolean;
  hasPolicy: boolean;
  allowedRoots: string[];
  terminals: number;
};
type PortRow = { port: number; allowed: boolean };
type TerminalRow = {
  id: string;
  shell: string;
  cwd: string;
  started: string;
  alive: boolean;
};
type AuditEntry = { at: string; event: string; detail?: string; allowed: boolean };

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path);
  if (!res.ok) throw new Error(`${path}: ${res.status}`);
  return (await res.json()) as T;
}

function Card(props: { title: string; children: any }) {
  return (
    <section class="rounded-xl border border-zinc-800 bg-zinc-900/60 p-4 shadow">
      <h2 class="mb-2 text-sm font-semibold uppercase tracking-wide text-zinc-400">
        {props.title}
      </h2>
      {props.children}
    </section>
  );
}

export default function App() {
  const [system, setSystem] = createSignal<SystemInfo | null>(null);
  const [ports, setPorts] = createSignal<PortRow[]>([]);
  const [terminals, setTerminals] = createSignal<TerminalRow[]>([]);
  const [audit, setAudit] = createSignal<AuditEntry[]>([]);
  const [cwd, setCwd] = createSignal("");
  const [error, setError] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  const [output, setOutput] = createSignal<Record<string, string>>({});

  const refresh = async () => {
    try {
      setError("");
      setSystem(await getJSON<SystemInfo>("/api/system"));
      setPorts((await getJSON<{ ports: PortRow[] }>("/api/ports")).ports ?? []);
      setTerminals((await getJSON<{ terminals: TerminalRow[] }>("/api/terminals")).terminals ?? []);
    } catch (e) {
      setError(String(e));
    }
  };

  const refreshOutput = async (id: string) => {
    try {
      const o = await getJSON<{ output: string }>(`/api/terminal/${id}/output`);
      setOutput((prev) => ({ ...prev, [id]: o.output }));
    } catch {
      /* stale session */
    }
  };

  const action = async (body: Record<string, unknown>) => {
    setBusy(true);
    setError("");
    try {
      const res = await fetch("/api/action", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        const t = await res.json().catch(() => ({}));
        throw new Error((t as any).error ?? `${res.status}`);
      }
      await refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  createEffect(() => {
    void refresh();
    void getJSON<{ entries: AuditEntry[] }>("/api/audit")
      .then((r) => setAudit(r.entries ?? []))
      .catch(() => setAudit([]));
    const tick = setInterval(() => {
      void refresh();
      for (const t of terminals()) void refreshOutput(t.id);
    }, 5000);
    onCleanup(() => clearInterval(tick));
  });

  return (
    <main class="mx-auto flex max-w-2xl flex-col gap-4 p-4 pb-16">
      <header class="flex items-baseline justify-between">
        <h1 class="text-xl font-bold">ormos ui</h1>
        <Show when={system()}>
          <span class="text-sm text-zinc-400">
            {system()!.hostname} · v{system()!.version}
          </span>
        </Show>
      </header>
      <Show when={error()}>
        <p class="rounded-lg border border-red-800 bg-red-950/60 p-3 text-sm text-red-300">
          {error()}
        </p>
      </Show>

      <Show when={system()}>
        <Card title="This machine">
          <dl class="grid grid-cols-2 gap-x-4 gap-y-1 text-sm">
            <dt class="text-zinc-500">os</dt>
            <dd>{system()!.os}/{system()!.arch}</dd>
            <dt class="text-zinc-500">shell</dt>
            <dd class="font-mono">{system()!.shell}</dd>
            <dt class="text-zinc-500">state dir</dt>
            <dd class="truncate font-mono">{system()!.configDir}</dd>
            <dt class="text-zinc-500">policy</dt>
            <dd>{system()!.hasPolicy ? "active" : "none (agent defaults)"}</dd>
          </dl>
        </Card>
      </Show>

      <Card title="Listening ports">
        <Show when={ports().length > 0} fallback={<p class="text-sm text-zinc-500">No policy-allowed listeners.</p>}>
          <ul class="divide-y divide-zinc-800 text-sm font-mono">
            <For each={ports()}>
              {(p) => (
                <li class="flex justify-between py-1.5">
                  <span>:{p.port}</span>
                  <span class={p.allowed ? "text-emerald-400" : "text-zinc-500"}>
                    {p.allowed ? "exposable" : "blocked by policy"}
                  </span>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </Card>

      <Card title="Terminals">
        <div class="mb-3 flex gap-2">
          <input
            class="min-w-0 flex-1 rounded-lg border border-zinc-700 bg-zinc-950 px-3 py-2 font-mono text-sm"
            placeholder="cwd (optional)"
            value={cwd()}
            onInput={(e) => setCwd(e.currentTarget.value)}
          />
          <button
            class="rounded-lg bg-emerald-600 px-4 py-2 text-sm font-semibold disabled:opacity-40"
            disabled={busy()}
            onClick={() => void action({ action: "open", cwd: cwd() || undefined })}
          >
            Open
          </button>
        </div>
        <Show when={terminals().length > 0} fallback={<p class="text-sm text-zinc-500">No terminals opened from this UI.</p>}>
          <ul class="flex flex-col gap-3">
            <For each={terminals()}>
              {(t) => (
                <li class="rounded-lg border border-zinc-800 bg-zinc-950 p-3">
                  <div class="mb-1 flex items-center justify-between text-sm">
                    <span class="font-mono">{t.shell} · {t.cwd}</span>
                    <button
                      class="rounded-md border border-red-800 px-2 py-1 text-xs text-red-300"
                      disabled={!t.alive || busy()}
                      onClick={() => void action({ action: "kill", id: t.id })}
                    >
                      kill
                    </button>
                  </div>
                  <pre class="max-h-48 overflow-auto whitespace-pre-wrap font-mono text-xs text-zinc-300">
                    {output()[t.id] ?? "…"}
                  </pre>
                  <div class="mt-1 text-xs text-zinc-500">
                    {t.alive ? "alive" : "exited"} · {t.id}
                  </div>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </Card>

      <Card title="Agent audit (sessions.log tail)">
        <Show when={audit().length > 0} fallback={<p class="text-sm text-zinc-500">No audit entries yet.</p>}>
          <ul class="max-h-64 space-y-1 overflow-auto font-mono text-xs">
            <For each={audit()}>
              {(a) => (
                <li class="flex gap-2">
                  <span class="shrink-0 text-zinc-500">{a.at.slice(11, 19)}</span>
                  <span class={a.allowed ? "text-zinc-300" : "text-amber-400"}>
                    {a.event}{a.detail ? ` — ${a.detail}` : ""}
                  </span>
                </li>
              )}
            </For>
          </ul>
        </Show>
      </Card>

      <footer class="text-center text-xs text-zinc-600">
        Loopback by default · no auth — reachability is the boundary · actions are
        server-allowlisted
      </footer>
    </main>
  );
}
