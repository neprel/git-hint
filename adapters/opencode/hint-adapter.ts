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
// Store rule: the plugin never writes files. Its counters go through the child, which
// checks the Git exclusion of .hint/ before every write. When the child stops (the
// exclusion was removed or a file below .hint/ entered the Git index), it says so on its
// stdout; the plugin then stops sending and counts the later events as dropped. The
// counters always stay in memory (globalThis[Symbol.for("hint.opencode-adapter.stats")]).
//
// Environment (optional): HINT_BIN (path of git-hint; default "git-hint" on PATH),
// HINT_DISABLE=1 (turn the adapter off), HINT_MCP_SERVER (the name of the git-hint MCP server
// in opencode.json; default "githint"), HINT_OPENCODE_SESSION (the root OpenCode session of
// the run, for example started with `opencode run --session <id>`; model usage is recorded
// only for that session and its observed child sessions, never for other sessions of a shared
// OpenCode server; without it model usage stays unknown).

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

// HINT context measurements (hint_context of the git-hint MCP server). Only a text part that
// is entirely one marker line, and only for the HINT tool names, is read; it holds metadata
// only (enumerations, numbers, opaque IDs). Anything else is no measurement: a marker inside
// file or shell output is ignored, a damaged one is reported as invalid (missing coverage).
const MARKER = "hint-context-measurement/1 "
const MAX_MARKER = 2048
const CONTEXT_TOOLS = new Set(["githint_hint_context", `${process.env.HINT_MCP_SERVER || "githint"}_hint_context`])
const HEX32 = /^[0-9a-f]{32}$/
const ENUMS: Record<string, string[]> = {
  action: ["deliver", "expand", "retrieve_original"],
  stage: ["raw_tool_output"],
  policy_state: ["enabled", "disabled", "not_applicable", "unknown"],
  freshness: ["current", "stale", "unknown"],
  result: ["delivered", "refused_stale", "missing", "error"],
}
const COUNT = (v: unknown) => Number.isInteger(v) && (v as number) >= 0 && (v as number) <= 1e9

function validContext(c: any): boolean {
  if (!c || typeof c !== "object" || Array.isArray(c)) return false
  const allowed = new Set([...Object.keys(ENUMS), "artifact_id", "ref_id", "snapshot_id", "policy_ref", "bytes", "omitted", "tokens"])
  for (const k of Object.keys(c)) if (!allowed.has(k)) return false
  for (const [k, vals] of Object.entries(ENUMS)) if (!(k in c) ? k !== "freshness" : !vals.includes(c[k])) return false
  for (const k of ["artifact_id", "ref_id", "snapshot_id", "policy_ref"]) if (k in c && !HEX32.test(String(c[k]))) return false
  if (!COUNT(c.bytes) || ("omitted" in c && !COUNT(c.omitted))) return false
  const t = c.tokens
  if (t !== undefined) {
    if (!t || typeof t !== "object" || Object.keys(t).sort().join() !== "estimator,estimator_version,label,value") return false
    if (!COUNT(t.value) || t.estimator !== "bytes-div-4" || !/^[0-9]{1,4}(\.[0-9]{1,4}){0,2}$/.test(String(t.estimator_version)) || t.label !== "estimate")
      return false
  }
  return true
}

// contextOf returns the validated measurement of a HINT context call, "invalid", or undefined.
function contextOf(tool: string, result: any): { targets: string[]; context: any } | "invalid" | undefined {
  if (!CONTEXT_TOOLS.has(tool)) return undefined
  const parts = Array.isArray(result?.content) ? result.content : []
  const marks = parts.filter((p: any) => p?.type === "text" && typeof p.text === "string" && p.text.startsWith(MARKER))
  if (marks.length !== 1) return "invalid"
  const text: string = marks[0].text
  if (byteLen(text) > MAX_MARKER || text.includes("\n")) return "invalid"
  try {
    const m = JSON.parse(text.slice(MARKER.length))
    if (!m || typeof m !== "object" || Object.keys(m).sort().join() !== "context,targets") return "invalid"
    if (!Array.isArray(m.targets) || m.targets.length > 4 || !m.targets.every((x: unknown) => HEX32.test(String(x)))) return "invalid"
    if (!validContext(m.context)) return "invalid"
    return { targets: m.targets, context: m.context }
  } catch {
    return "invalid"
  }
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
  const c = ev?.status === "completed" ? contextOf(String(ev?.tool ?? ""), ev?.result) : undefined
  if (c === "invalid") out.context_invalid = true
  else if (c) out.context = c
  return { ...base("tool.execute.after", ev), output: out }
}

// Model calls (draft v0.2, OpenCode 2.0.26; 07B-E §30-§33): the plugin forwards metadata
// only — the bus event ID, type, session, parent, assistant message, usage source and the
// numeric token counters, and the kind of each model request. Never text, prompts, headers,
// request bodies or model names. Events of the allowlisted types only.
const MODEL_EVENTS = new Set([
  "session.created", "session.step.started", "session.step.ended", "session.step.failed", "session.retry.scheduled",
  "session.usage.recorded", "session.compaction.started", "session.compaction.ended", "session.compaction.failed",
])
const REQUEST_KINDS = new Set(["primary", "title", "compaction", "generate"])
const ID = (v: unknown) => (typeof v === "string" && v.length > 0 && v.length <= 200 ? v : undefined)
const NUM = (v: unknown) => (typeof v === "number" ? v : undefined)

function reduceTokens(t: any) {
  if (!t || typeof t !== "object") return undefined
  return { input: NUM(t.input), output: NUM(t.output), reasoning: NUM(t.reasoning), cache: { read: NUM(t.cache?.read), write: NUM(t.cache?.write) } }
}

function reduceEvent(e: any) {
  const d = e?.data ?? {}
  if (!MODEL_EVENTS.has(String(e?.type))) return undefined
  const out: Record<string, unknown> = { id: ID(e?.id), type: e.type, sessionID: ID(d.sessionID) }
  if (ID(d.parentID)) out.parentID = d.parentID
  if (ID(d.assistantMessageID)) out.assistantMessageID = d.assistantMessageID
  if (d.source === "title" || d.source === "compaction") out.source = d.source
  const t = reduceTokens(d.tokens)
  if (t) out.tokens = t
  return { hook: "event", t_wall_ms: Date.now(), event: out }
}

type Stats = { sent: number; dropped: number; hookErrors: number; childExit: number | null; stopped: boolean }
const STATS_KEY = Symbol.for("hint.opencode-adapter.stats")

export default {
  id: "hint-adapter",
  setup: async (ctx: any) => {
    const stats: Stats = { sent: 0, dropped: 0, hookErrors: 0, childExit: null, stopped: false }
    ;((globalThis as any)[STATS_KEY] ??= []).push(stats)
    const dir = String(ctx?.location?.project?.directory ?? ctx?.location?.directory ?? "")
    const bin = process.env.HINT_BIN || "git-hint"
    // model calls only when this OpenCode exposes both channels; the child still checks the version
    const modelCalls = typeof ctx?.event?.subscribe === "function" && typeof ctx?.session?.hook === "function"
    let child: any = null
    try {
      if (dir && process.env.HINT_DISABLE !== "1") {
        child = Bun.spawn([bin, "adapter", "opencode", "--project", dir, ...(modelCalls ? ["--model-calls"] : [])], {
          stdin: "pipe",
          stdout: "pipe",
          stderr: "ignore",
          env: { ...process.env }, // HINT_TASK_ID, HINT_AGENT_VERSION and the other declarations as they are now
        })
        child.exited.then((code: number) => {
          stats.childExit = code
        })
        // read the child's notices in the background; never awaited by a hook
        ;(async () => {
          try {
            const dec = new TextDecoder()
            let buf = ""
            for await (const chunk of child.stdout) {
              buf += dec.decode(chunk, { stream: true })
              if (buf.includes('"hint":"stopped"')) stats.stopped = true
              if (buf.length > 4096) buf = buf.slice(-256)
            }
          } catch {}
        })()
      }
    } catch {
      child = null
    }

    function write(line: string): boolean {
      try {
        child.stdin.write(line)
        const r = child.stdin.flush()
        if (r && typeof r.catch === "function") r.catch(() => {})
        return true
      } catch {
        return false
      }
    }

    // Counters go to the child after each finished call (a cleanup call at process exit is
    // not guaranteed); the child stores them only while .hint/ is excluded.
    function writeStats() {
      if (!child || stats.childExit !== null || stats.stopped) return
      write(JSON.stringify({ hook: "hint.stats", stats }) + "\n")
    }

    function send(ev: Record<string, unknown>) {
      if (!child || stats.childExit !== null || stats.stopped) {
        stats.dropped++
        return
      }
      if (write(JSON.stringify(ev) + "\n")) stats.sent++
      else stats.dropped++
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

    // the subscription first, then the attach line: sessions created before it are mid-session
    const abort = new AbortController()
    let drained: Promise<boolean> = Promise.resolve(true)
    if (modelCalls) {
      try {
        const events = ctx.event.subscribe({ signal: abort.signal })
        drained = (async () => {
          try {
            for await (const e of events) {
              try {
                const r = reduceEvent(e)
                if (r) send(r)
              } catch {
                stats.hookErrors++
              }
            }
            return true
          } catch {
            return abort.signal.aborted // an abort ends the iteration; anything else is a stream error
          }
        })()
        send({ hook: "hint.attach", t_wall_ms: Date.now() })
        await ctx.session.hook("model.request", async (ev: any) => {
          try {
            const kind = String(ev?.kind ?? "")
            if (REQUEST_KINDS.has(kind)) send({ hook: "model.request", t_wall_ms: Date.now(), input: { sessionID: String(ev?.sessionID ?? ""), kind } })
          } catch {
            stats.hookErrors++
          }
        })
      } catch {
        stats.hookErrors++
      }
    }

    return async () => {
      try {
        if (child && stats.childExit === null) {
          if (modelCalls) {
            // stop and drain the subscription, then the closure line; the child acknowledges it
            // only when every preceding line was written
            abort.abort()
            const ok = await Promise.race([drained, new Promise<boolean>((r) => setTimeout(() => r(false), 2000))])
            send({ hook: "hint.close", t_wall_ms: Date.now(), close: { drained: ok === true, dropped: stats.dropped, errors: stats.hookErrors } })
          }
          writeStats()
          child.stdin.end()
          await Promise.race([child.exited, new Promise((r) => setTimeout(r, 2000))])
        }
      } catch {}
    }
  },
}
