import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

// Ensures the `mcp` binary is installed when running under pi.
//
// This is pi's analog of the Claude Code SessionStart hook (see hooks/hooks.json):
// on each session start it runs scripts/install.sh, which is idempotent — it exits
// 0 immediately when `mcp` is already on PATH — so this is a cheap no-op after the
// first run. The script downloads the release binary matching the host OS/arch.
//
// POSIX only (macOS/Linux); Windows users should install the binary manually.
export default function (pi: ExtensionAPI) {
  pi.on("session_start", async (_event, ctx) => {
    const installScript = join(
      dirname(fileURLToPath(import.meta.url)),
      "..",
      "scripts",
      "install.sh",
    );
    try {
      await pi.exec("sh", [installScript]);
    } catch {
      ctx.ui.notify(
        `mcp CLI not found and auto-install failed — run \`sh ${installScript}\` manually`,
        "error",
      );
    }
  });
}
