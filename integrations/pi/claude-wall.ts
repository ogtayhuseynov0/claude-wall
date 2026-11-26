// claude-wall status bridge for Pi (https://github.com/earendil-works/pi).
//
// Installed by `claude-wall init` to ~/.pi/agent/extensions/claude-wall.ts.
// It posts Pi's agent events to the local claude-wall server in the shape of
// Claude Code hook events, so a Pi pane shows working / waiting / idle on the
// wall like a Claude Code pane. It does nothing outside tmux.
//
// Port: CLAUDE_WALL_PORT (default 7685).

const env: Record<string, string | undefined> = (globalThis as any).process?.env ?? {};
const PANE = env.TMUX_PANE || "";
const URL = `http://127.0.0.1:${env.CLAUDE_WALL_PORT || "7685"}/api/hooks/event`;
const HEARTBEAT_MS = 20_000;

// Pi's built-in tools, named the way claude-wall formats Claude Code tools.
const TOOLS: Record<string, string> = { bash: "Bash", read: "Read", edit: "Edit", write: "Write", grep: "Grep", find: "Glob", ls: "LS" };

function toolEvent(name: string, args: any) {
  const a = args && typeof args === "object" ? args : {};
  const tool = TOOLS[name] || name;
  const input: Record<string, string> = {};
  if (typeof a.command === "string") input.command = a.command;
  if (typeof a.path === "string") input.file_path = a.path;
  if (typeof a.pattern === "string") input.pattern = a.pattern;
  return { tool_name: tool, tool_input: input };
}

export default function (pi: any) {
  if (!PANE) return;
  // One id per pane: /new and /resume keep the same row on the wall.
  const sessionId = "pi-" + PANE.replace(/[^0-9]/g, "");
  let cwd: string = (globalThis as any).process?.cwd?.() ?? "";
  let queue: Promise<unknown> = Promise.resolve();
  let beat: ReturnType<typeof setInterval> | null = null;

  // Events go out one at a time, in order (claude-wall relies on that).
  const send = (name: string, extra: Record<string, unknown> = {}) => {
    const body = JSON.stringify({ session_id: sessionId, cwd, hook_event_name: name, agent: "pi", ...extra });
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

  pi.on("session_start", async (_event: any, ctx: any) => {
    if (ctx?.cwd) cwd = ctx.cwd;
    send("SessionStart");
  });
  pi.on("agent_start", async () => {
    busy(true);
    send("UserPromptSubmit");
  });
  pi.on("tool_execution_start", async (event: any) => {
    send("PreToolUse", toolEvent(event.toolName, event.args));
  });
  pi.on("tool_execution_end", async (event: any) => {
    send(event.isError ? "PostToolUseFailure" : "PostToolUse", toolEvent(event.toolName, event.args));
  });
  pi.on("session_before_compact", async () => {
    send("PreCompact");
  });
  // Compaction ends: back to the run, or idle after a manual /compact.
  const compacted = async () => send(beat ? "UserPromptSubmit" : "Idle");
  pi.on("session_compact", compacted);
  pi.on("session_compact_failed", compacted);
  // An extension dialog (confirm, select, input) waits for the user.
  pi.on("ui_prompt_start", async (event: any) => {
    send("PermissionRequest", { tool_name: event?.title || "Waiting for input" });
  });
  pi.on("ui_prompt_end", async () => {
    send(beat ? "PostToolUse" : "Idle");
  });
  // agent_end can be followed by a retry or a queued message; agent_settled is final.
  pi.on("agent_settled", async () => {
    busy(false);
    send("Stop");
  });
  pi.on("session_shutdown", async (event: any) => {
    if (event?.reason !== "quit") return;
    busy(false);
    send("SessionEnd");
    await queue;
  });
}
