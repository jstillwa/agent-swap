// agent-swap pi extension: reports session state to `agent-swap hook <Event>`.
// Written by agent-swap and loaded with `pi -e`; it must never throw.
import { spawnSync } from "node:child_process";

export default function (pi: any) {
  let sessionId = "";
  let lastError = "";
  const hook = (event: string, error = "") => {
    try {
      const exe = process.env.AGENT_SWAP_EXE;
      if (!exe) return;
      spawnSync(exe, ["hook", event], {
        input: JSON.stringify({ session_id: sessionId, error }),
        stdio: ["pipe", "ignore", "ignore"],
        timeout: 10000,
      });
    } catch {}
  };
  pi.on("session_start", async (_e: any, ctx: any) => {
    try {
      sessionId = ctx.sessionManager.getSessionId() ?? "";
    } catch {}
    hook("SessionStart");
  });
  pi.on("agent_start", async () => {
    lastError = "";
    hook("UserPromptSubmit");
  });
  pi.on("agent_end", async (e: any) => {
    try {
      const last = [...(e?.messages ?? [])].reverse().find((m: any) => m?.role === "assistant");
      lastError = last?.stopReason === "error" ? String(last.errorMessage ?? "error") : "";
    } catch {}
  });
  pi.on("agent_settled", async () => {
    if (lastError) hook("StopFailure", lastError);
    else hook("Stop");
  });
}
