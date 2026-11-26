// claude-wall status bridge for OpenCode: the TUI part.
//
// Installed by `claude-wall init` to ~/.config/opencode/plugins/claude-wall/.
// It watches the session this terminal shows and posts its events to the
// local claude-wall server in the shape of Claude Code hook events, so an
// OpenCode pane shows working / waiting / idle on the wall like a Claude Code
// pane. It does nothing outside tmux.
//
// Port: CLAUDE_WALL_PORT (default 7685).

const env: Record<string, string | undefined> = (globalThis as any).process?.env ?? {};
const PANE = env.TMUX_PANE || "";
const URL = `http://127.0.0.1:${env.CLAUDE_WALL_PORT || "7685"}/api/hooks/event`;
const HEARTBEAT_MS = 20_000;
const ROUTE_POLL_MS = 1_000;

// OpenCode's tools, named the way claude-wall formats Claude Code tools.
const TOOLS: Record<string, string> = {
  shell: "Bash",
  bash: "Bash",
  read: "Read",
  edit: "Edit",
  write: "Write",
  patch: "Edit",
  apply_patch: "Edit",
  grep: "Grep",
  glob: "Glob",
  list: "LS",
  task: "Agent",
  webfetch: "WebFetch",
  websearch: "WebSearch",
};

function toolEvent(name: string, args: any) {
  const a = args && typeof args === "object" ? args : {};
  const input: Record<string, string> = {};
  if (typeof a.command === "string") input.command = a.command;
  const file = a.filePath ?? a.file_path ?? a.path;
  if (typeof file === "string") input.file_path = file;
  if (typeof a.pattern === "string") input.pattern = a.pattern;
  if (typeof a.description === "string") input.description = a.description;
  if (typeof a.query === "string") input.query = a.query;
  return { tool_name: TOOLS[name] || name || "tool", tool_input: input };
}

export default {
  id: "claude-wall",
  setup(context: any) {
    if (!PANE) return;
    // One id per pane: switching sessions in the TUI keeps the same row on the wall.
    const sessionId = "opencode-" + PANE.replace(/[^0-9]/g, "");
    const cwd: string = context.location?.directory ?? (globalThis as any).process?.cwd?.() ?? "";
    const names = new Map<string, string>(); // tool call id → tool name
    let queue: Promise<unknown> = Promise.resolve();
    let beat: ReturnType<typeof setInterval> | null = null;
    let shown = ""; // the session this terminal shows

    // Events go out one at a time, in order (claude-wall relies on that).
    const send = (name: string, extra: Record<string, unknown> = {}) => {
      const body = JSON.stringify({ session_id: sessionId, cwd, hook_event_name: name, agent: "opencode", ...extra });
      queue = queue
        .then(() =>
          fetch(URL, {
            method: "POST",
            headers: { "Content-Type": "application/json", "X-Tmux-Pane": PANE },
            body,
            signal: AbortSignal.timeout(2000),
          }),
        )
        .catch(() => {});
    };
    const busy = (on: boolean) => {
      if (on && !beat) {
        beat = setInterval(() => send("Heartbeat"), HEARTBEAT_MS);
        (beat as any).unref?.();
      } else if (!on && beat) {
        clearInterval(beat);
        beat = null;
      }
    };

    const route = (): string => {
      try {
        const r = context.ui.router.current();
        return r?.type === "session" ? r.sessionID : "";
      } catch {
        return "";
      }
    };
    const root = (id: string): string => {
      try {
        return context.data.session.root(id) || id;
      } catch {
        return id;
      }
    };
    // Events from the shown session or its subagent sessions.
    const ours = (id: string | undefined) => !!id && !!shown && (id === shown || root(id) === root(shown));

    // The shown session changed: report where it stands now.
    const follow = () => {
      const next = route();
      if (next === shown) return;
      shown = next;
      let running = false;
      try {
        running = !!next && context.data.session.status(next) === "running";
      } catch {}
      busy(running);
      send(running ? "UserPromptSubmit" : "Idle");
    };

    const handle = (event: any) => {
      const d = event?.data ?? {};
      switch (event?.type) {
        case "session.execution.started":
          if (!ours(d.sessionID)) return;
          if (d.sessionID === shown) {
            busy(true);
            send("UserPromptSubmit");
          } else send("SubagentStart");
          return;
        case "session.tool.input.started":
          if (ours(d.sessionID)) names.set(d.id, d.name);
          return;
        case "session.tool.called":
          if (ours(d.sessionID)) send("PreToolUse", toolEvent(names.get(d.id) ?? "", d.input));
          return;
        case "session.tool.success":
        case "session.tool.failed":
          if (!ours(d.sessionID)) return;
          send(event.type === "session.tool.failed" ? "PostToolUseFailure" : "PostToolUse", toolEvent(names.get(d.id) ?? "", {}));
          names.delete(d.id);
          return;
        case "session.compaction.started":
          if (ours(d.sessionID)) send("PreCompact");
          return;
        case "session.compaction.ended":
        case "session.compaction.failed":
          // Back to the run, or idle after a manual compaction.
          if (ours(d.sessionID)) send(beat ? "UserPromptSubmit" : "Idle");
          return;
        case "permission.asked": {
          if (!ours(d.sessionID)) return;
          const res: string[] = Array.isArray(d.resources) ? d.resources : [];
          const args = d.action === "shell" || d.action === "bash" ? { command: res.join(" ") } : { path: res[0] };
          send("PermissionRequest", toolEvent(d.action, args));
          return;
        }
        case "form.created":
          if (ours(d.form?.sessionID)) send("PermissionRequest", { tool_name: d.form?.title || "Question" });
          return;
        case "permission.replied":
        case "form.replied":
        case "form.cancelled":
          if (ours(d.sessionID)) send(beat ? "PostToolUse" : "Idle");
          return;
        case "session.execution.succeeded":
        case "session.execution.interrupted":
        case "session.execution.failed":
          if (!ours(d.sessionID)) return;
          if (d.sessionID !== shown) {
            send("SubagentStop");
            return;
          }
          busy(false);
          send(event.type === "session.execution.failed" ? "StopFailure" : "Stop");
          return;
        case "session.idle":
          // A fallback for a missed end of execution.
          if (d.sessionID === shown && beat) {
            busy(false);
            send("Stop");
          }
          return;
      }
    };

    send("SessionStart");
    follow();
    const stopEvents = context.data.listen(({ details }: any) => handle(details));
    const poll = setInterval(follow, ROUTE_POLL_MS);
    (poll as any).unref?.();

    return async () => {
      clearInterval(poll);
      stopEvents?.();
      busy(false);
      send("SessionEnd");
      await queue;
    };
  },
};
