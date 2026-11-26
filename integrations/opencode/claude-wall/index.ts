// claude-wall status bridge for OpenCode: the server part.
//
// The work happens in tui.ts, because only the terminal client knows its tmux
// pane (the shared OpenCode server runs outside it). OpenCode lists a plugin
// folder only when it has a server part, so this one does nothing.
export default {
  id: "claude-wall",
  setup() {},
}
