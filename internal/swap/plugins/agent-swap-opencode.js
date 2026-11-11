// agent-swap plugin for opencode: forwards session state to `agent-swap hook <Event>`.
import { spawnSync } from "node:child_process";

const hook = (event, sessionID, error) => {
  try {
    const exe = process.env.AGENT_SWAP_EXE;
    if (!exe) return;
    const input = JSON.stringify({ session_id: sessionID || "", error: error || "" });
    spawnSync(exe, ["hook", event], { input, stdio: ["pipe", "ignore", "ignore"], timeout: 10000 });
  } catch {}
};

const errorText = (e) => {
  if (!e) return "";
  const d = e.data || {};
  return [e.name, d.message, d.statusCode].filter((x) => x !== undefined && x !== "").join(" ");
};

// Track only the main session: subagent (task tool) sessions have a parentID.
let main;
const children = new Set();
// session.idle (sent twice) follows session.error: skip Stop until the next busy so the failure stands.
const failed = new Set();
// Rate limits arrive as status "retry" (opencode backs off and retries, alternating with busy);
// keep the limit flagged by skipping busy until the turn ends.
const retrying = new Set();

export const AgentSwap = async () => ({
  event: async ({ event }) => {
    try {
      const p = event.properties || {};
      const info = p.info;
      if ((event.type === "session.created" || event.type === "session.updated") && info?.parentID) children.add(info.id);
      // A new top-level session (/new) becomes the main one; agent-swap validates the switch.
      if (event.type === "session.created" && info?.id && !info.parentID) main = info.id;
      const id = p.sessionID;
      if (!id || children.has(id)) return;
      main ??= id;
      if (id !== main) return;
      const status = p.status?.type;
      if (event.type === "session.status" && status === "busy") {
        failed.delete(id);
        if (!retrying.has(id)) hook("UserPromptSubmit", id);
      } else if (event.type === "session.status" && status === "retry") {
        retrying.add(id);
        if ((p.status.attempt || 0) >= 3) hook("LimitHit", id, p.status.message);
      } else if (event.type === "session.idle") {
        retrying.delete(id);
        if (!failed.has(id)) hook("Stop", id);
      } else if (event.type === "session.error") {
        retrying.delete(id);
        failed.add(id);
        hook("StopFailure", id, errorText(p.error));
      }
    } catch {}
  },
});
