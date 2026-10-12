// HINT adapter for OpenCode (plugin API of OpenCode 2.x; checked with 2.0.26).
// Install: `git hint adapter install opencode` (writes this file to .opencode/plugins/).
// Put only one HINT adapter file in .opencode/plugins/.
//
// Data rule: tool output never leaves this process. The plugin keeps an allowlist of
// argument fields, replaces output text by its byte length, and streams the reduced
// events through a pipe to one long-lived child process (`git-hint adapter opencode`).
// The child turns them into HINT observations with opaque IDs; only those reach the disk.
//
// Safety rule: no hook throws, awaits the child, or changes the hook event. Without the
// child (git-hint missing, project not initialized) the plugin drops and counts events.
//
// Environment (optional): HINT_BIN (path of git-hint; default "git-hint" on PATH),
// HINT_DISABLE=1 (turn the adapter off).

const ARG_ALLOW = ["filePath", "path", "offset", "limit", "command", "workdir"]

function pick(src: any): Record<string, unknown> | undefined {
  if (!src || typeof src !== "object") return undefined
  const out: Record<string, unknown> = {}
  for (const k of ARG_ALLOW) {
    const v = src[k]
    if (typeof v === "string" || typeof v === "number" || typeof v === "boolean") out[k] = v
  }
  return out
}

function byteLen(s: string): number {
  return new TextEncoder().encode(s).length
}

// contentLen measures what the model receives, without copying it.
function contentLen(result: any): number | undefined {
  const c = result?.content
  if (typeof c === "string") return byteLen(c)
  if (Array.isArray(c)) {
    let n = 0
    for (const part of c) if (part && part.type === "text" && typeof part.text === "string") n += byteLen(part.text)
    return n
  }
  if (typeof result?.output === "string") return byteLen(result.output)
  return undefined
}

function base(hook: string, ev: any) {
  return {
    hook,
    t_wall_ms: Date.now(),
    input: { tool: String(ev?.tool ?? ""), sessionID: String(ev?.sessionID ?? ""), callID: String(ev?.id ?? "") },
  }
}

function reduceBefore(ev: any) {
  return { ...base("tool.execute.before", ev), output: { args: pick(ev?.input) } }
}

function reduceAfter(ev: any) {
  const metadata: Record<string, unknown> = {}
  const md = ev?.status === "completed" ? ev?.result?.metadata ?? {} : ev?.error?.metadata ?? {}
  if (typeof md.exit === "number") metadata.exit = md.exit
  if (Array.isArray(md.files))
    metadata.files = md.files.map((f: any) => {
      const p = [f?.file, f?.filePath, f?.path].find((x) => typeof x === "string")
      return { filePath: p ?? "" }
    })
  const out: Record<string, unknown> = { metadata, status: ev?.status === "error" ? "error" : "completed" }
  const n = ev?.status === "completed" ? contentLen(ev?.result) : undefined
  if (n !== undefined) out.output = `[redacted:len=${n}]`
  return { ...base("tool.execute.after", ev), output: out }
}

type Stats = { sent: number; dropped: number; hookErrors: number; childExit: number | null }
const STATS_KEY = Symbol.for("hint.opencode-adapter.stats")

export default {
  id: "hint-adapter",
  setup: async (ctx: any) => {
    const stats: Stats = { sent: 0, dropped: 0, hookErrors: 0, childExit: null }
    ;((globalThis as any)[STATS_KEY] ??= []).push(stats)
    const dir = String(ctx?.location?.project?.directory ?? ctx?.location?.directory ?? "")
    const bin = process.env.HINT_BIN || "git-hint"
    let child: any = null
    try {
      if (dir && process.env.HINT_DISABLE !== "1") {
        child = Bun.spawn([bin, "adapter", "opencode", "--project", dir], {
          stdin: "pipe",
          stdout: "ignore",
          stderr: "ignore",
        })
        child.exited.then((code: number) => {
          stats.childExit = code
        })
      }
    } catch {
      child = null
    }

    // Counters go to the local store after each finished call, because a cleanup call at
    // process exit is not guaranteed. Only when the store exists.
    function writeStats() {
      try {
        const fs = require("node:fs")
        const p = require("node:path").join(dir, ".hint", "state", "activity")
        if (!fs.existsSync(p)) return
        fs.writeFileSync(require("node:path").join(p, "adapter-stats.json"), JSON.stringify(stats) + "\n", { mode: 0o600 })
      } catch {}
    }

    function send(ev: Record<string, unknown>) {
      if (!child || stats.childExit !== null) {
        stats.dropped++
        return
      }
      try {
        child.stdin.write(JSON.stringify(ev) + "\n")
        const r = child.stdin.flush()
        if (r && typeof r.catch === "function") r.catch(() => stats.dropped++)
        stats.sent++
      } catch {
        stats.dropped++
      }
    }

    try {
      await ctx.tool.hook("execute.before", async (ev: any) => {
        try {
          send(reduceBefore(ev))
        } catch {
          stats.hookErrors++
        }
      })
      await ctx.tool.hook("execute.after", async (ev: any) => {
        try {
          send(reduceAfter(ev))
        } catch {
          stats.hookErrors++
        }
        writeStats()
      })
    } catch {
      stats.hookErrors++
    }

    return async () => {
      try {
        if (child && stats.childExit === null) {
          child.stdin.end()
          await Promise.race([child.exited, new Promise((r) => setTimeout(r, 2000))])
        }
      } catch {}
      writeStats()
    }
  },
}
