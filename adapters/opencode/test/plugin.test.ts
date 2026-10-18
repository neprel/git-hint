// Offline test of the OpenCode adapter plugin. No OpenCode process and no model: the test
// registers the plugin against a stub ctx.tool.hook and replays synthetic hook events in
// the OpenCode 2.x shape. The child is the real git-hint binary. Run through run.sh.
import { describe, expect, test } from "bun:test"
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import adapter from "../hint-adapter.ts"

const BIN = process.env.HINT_TEST_BIN ?? ""
const PROJECT = process.env.HINT_TEST_PROJECT ?? ""
const CANARY = /HINTCANARY_|canary_dir_e1d2|canarySymbolQ7|ses_canary|call_canary/
const stats = () => (globalThis as any)[Symbol.for("hint.opencode-adapter.stats")] as any[]

function events(dir: string) {
  const sid = "ses_canary_1"
  const f = `${dir}/canary_dir_e1d2/app.go`
  const before = (id: string, tool: string, input: any) => ({ hook: "execute.before", event: { tool, sessionID: sid, agent: "build", messageID: "msg_canary", id, input } })
  const after = (id: string, tool: string, input: any, result: any) => ({ hook: "execute.after", event: { tool, sessionID: sid, agent: "build", messageID: "msg_canary", id, input, status: "completed", result } })
  const text = (s: string) => ({ content: [{ type: "text", text: s }], metadata: {} })
  return [
    before("call_canary_1", "read", { filePath: f }),
    after("call_canary_1", "read", { filePath: f }, text("package main // HINTCANARY_FILE_TEXT canarySymbolQ7")),
    before("call_canary_2", "grep", { pattern: "HINTCANARY_PATTERN", path: dir }),
    after("call_canary_2", "grep", { pattern: "HINTCANARY_PATTERN", path: dir }, text(`${f}:1: HINTCANARY_MATCH`)),
    before("call_canary_3", "edit", { filePath: f, oldString: "HINTCANARY_OLD", newString: "HINTCANARY_NEW" }),
    after("call_canary_3", "edit", { filePath: f }, { content: [], metadata: { files: [{ file: f, patch: "HINTCANARY_PATCH" }] } }),
    before("call_canary_4", "bash", { command: "go test ./... # HINTCANARY_CMD", workdir: dir }),
    after("call_canary_4", "bash", { command: "go test ./..." }, { content: [{ type: "text", text: "FAIL HINTCANARY_OUTPUT" }], metadata: { exit: 1 } }),
    before("call_canary_5", "webfetch", { url: "https://HINTCANARY.invalid" }),
    { hook: "execute.after", event: { tool: "webfetch", sessionID: sid, id: "call_canary_5", status: "error", error: { message: "HINTCANARY_ERROR" } } },
    before("call_canary_6", "subagent", { prompt: "HINTCANARY_PROMPT do the task" }),
  ]
}

async function replay(recs: any[], dir: string, bin: string) {
  const saved = process.env.HINT_BIN
  process.env.HINT_BIN = bin
  const handlers: Record<string, (ev: any) => Promise<void>> = {}
  const ctx = {
    location: { directory: dir, project: { directory: dir, id: "p" } },
    tool: { hook: async (name: string, fn: any) => ((handlers[name] = fn), { dispose: async () => {} }) },
  }
  const cleanup: any = await adapter.setup(ctx)
  if (saved === undefined) delete process.env.HINT_BIN
  else process.env.HINT_BIN = saved
  const lat: number[] = []
  for (const r of recs) {
    const before = JSON.stringify(r.event)
    const t = performance.now()
    await handlers[r.hook](r.event)
    lat.push(performance.now() - t)
    expect(JSON.stringify(r.event)).toBe(before) // the hook never changes the event
  }
  await cleanup?.()
  return { lat, st: stats().at(-1) }
}

function spool(dir: string): any[] {
  const p = join(dir, ".hint", "state", "activity", "spool.jsonl")
  return existsSync(p) ? readFileSync(p, "utf8").split("\n").filter((l) => l.trim()).map((l) => JSON.parse(l)) : []
}

describe("OpenCode adapter plugin, offline", () => {
  test("binary and project are prepared", () => {
    expect(existsSync(BIN)).toBe(true)
    expect(existsSync(join(PROJECT, ".hint"))).toBe(true)
  })

  test("one observation per call, no content on disk", async () => {
    const recs = events(PROJECT)
    const { st } = await replay(recs, PROJECT, BIN)
    expect(st.dropped).toBe(0)
    expect(st.hookErrors).toBe(0)
    const obs = spool(PROJECT)
    expect(obs.length).toBe(6)
    const text = readFileSync(join(PROJECT, ".hint", "state", "activity", "spool.jsonl"), "utf8")
    expect(CANARY.test(text)).toBe(false)
    expect(text.includes(PROJECT)).toBe(false)
    const ops = obs.map((o) => o.op).sort()
    expect(ops).toEqual(["DELEGATE", "EDIT_INTENT", "READ", "SEARCH", "TEST", "TOOL"])
    for (const o of obs) for (const t of o.targets ?? []) expect(/^[0-9a-f]{32}$/.test(t.entity_id)).toBe(true)
    expect(statSync(join(PROJECT, ".hint", "state", "activity", "spool.jsonl")).mode & 0o777).toBe(0o600)
    // the counters reach the store only through the child (the plugin writes no file)
    const st2 = JSON.parse(readFileSync(join(PROJECT, ".hint", "state", "activity", "adapter-stats.json"), "utf8"))
    expect(st2.dropped).toBe(0)
    expect(st2.child.written).toBeGreaterThan(0)
  })

  test("the pipe carries no output, edit text, search pattern or prompt", async () => {
    const dir = mkdtempSync(join(tmpdir(), "hint-pipe-"))
    const cap = join(dir, "pipe.jsonl")
    const fake = join(dir, "capture.sh")
    writeFileSync(fake, `#!/bin/sh\ncat > "${cap}"\n`)
    chmodSync(fake, 0o755)
    try {
      await replay(events(PROJECT), PROJECT, fake)
      await Bun.sleep(100)
      const pipe = readFileSync(cap, "utf8")
      for (const bad of ["HINTCANARY_FILE_TEXT", "HINTCANARY_PATTERN", "HINTCANARY_MATCH", "HINTCANARY_OLD", "HINTCANARY_NEW", "HINTCANARY_PATCH", "HINTCANARY_OUTPUT", "HINTCANARY_ERROR", "HINTCANARY_PROMPT", "HINTCANARY.invalid", "msg_canary"])
        expect(pipe.includes(bad)).toBe(false)
      expect(pipe.includes("[redacted:len=")).toBe(true)
    } finally {
      rmSync(dir, { recursive: true, force: true })
    }
  })

  test("a missing binary drops and counts events, no hook error", async () => {
    const recs = events(PROJECT)
    const { st } = await replay(recs, PROJECT, "/nonexistent/git-hint")
    await Bun.sleep(50)
    expect(st.hookErrors).toBe(0)
    expect(st.sent + st.dropped).toBe(recs.length)
  })

  test("the plugin source writes no file", () => {
    const src = readFileSync(join(import.meta.dir, "..", "hint-adapter.ts"), "utf8")
    expect(/writeFile|appendFile|createWriteStream|node:fs|Bun\.write/.test(src)).toBe(false)
  })

  for (const change of ["rule removed", "file added to the index"])
    test(`exclusion changed during the session (${change}): no further write, events counted, no hook error`, async () => {
      const dir = mkdtempSync(join(tmpdir(), "hint-session-"))
      try {
        const sh = (cmd: string[], cwd = dir) => {
          const r = Bun.spawnSync(cmd, { cwd, env: process.env })
          if (r.exitCode !== 0) throw new Error(`${cmd.join(" ")}: ${r.stderr}`)
        }
        sh(["git", "init", "-q"])
        writeFileSync(join(dir, "a.go"), "package a\n")
        sh([BIN, "init"])
        const real = Bun.spawnSync(["pwd", "-P"], { cwd: dir }).stdout.toString().trim()
        const recs = events(real)
        const saved = process.env.HINT_BIN
        process.env.HINT_BIN = BIN
        const handlers: Record<string, (ev: any) => Promise<void>> = {}
        const ctx = { location: { directory: real, project: { directory: real, id: "p" } },
          tool: { hook: async (n: string, fn: any) => ((handlers[n] = fn), { dispose: async () => {} }) } }
        const cleanup: any = await adapter.setup(ctx)
        if (saved === undefined) delete process.env.HINT_BIN
        else process.env.HINT_BIN = saved
        const st = stats().at(-1)
        const spoolFile = join(real, ".hint", "state", "activity", "spool.jsonl")
        const statsFile = join(real, ".hint", "state", "activity", "adapter-stats.json")
        const count = () => (existsSync(spoolFile) ? readFileSync(spoolFile, "utf8").split("\n").filter((l) => l.trim()).length : 0)
        const wait = async (cond: () => boolean) => {
          for (let i = 0; i < 300 && !cond(); i++) await Bun.sleep(10)
        }
        await handlers[recs[0].hook](recs[0].event)
        await handlers[recs[1].hook](recs[1].event)
        await wait(() => count() === 1 && existsSync(statsFile))
        expect(count()).toBe(1)
        const statsBefore = readFileSync(statsFile, "utf8")
        if (change === "rule removed") writeFileSync(join(real, ".git", "info", "exclude"), "")
        else sh(["git", "add", "-f", ".hint/state/project.json"], real)
        for (const r of recs.slice(2, 6)) await handlers[r.hook](r.event)
        await wait(() => st.stopped)
        expect(st.stopped).toBe(true)
        for (const r of recs.slice(6)) await handlers[r.hook](r.event) // after the stop notice
        await cleanup?.()
        expect(count()).toBe(1)
        expect(readFileSync(statsFile, "utf8")).toBe(statsBefore)
        expect(st.hookErrors).toBe(0)
        expect(st.dropped).toBe(recs.length - 6)
        expect(st.childExit).toBe(4)
      } finally {
        rmSync(dir, { recursive: true, force: true })
      }
    })

  test("hint_context: the agent gets the context as text, the adapter only the measurement", async () => {
    // the real MCP answer, converted as a text-only client sees it: the text parts only
    const line = readFileSync(process.env.HINT_TEST_MCP ?? "", "utf8").split("\n").find((l) => l.includes('"id":2'))
    const result = JSON.parse(line ?? "{}").result
    expect(result.isError).not.toBe(true)
    const parts = result.content.filter((p: any) => p.type === "text")
    expect(parts.length).toBe(2)
    expect(parts[0].text).toContain("func Rate(a, b int) int") // useful context: source
    expect(parts[0].text).toMatch(/"ref":"hc1\.[0-9a-f]{32}\.[0-9a-f]{32}"/) // and expandable refs
    expect(parts[1].text.startsWith("hint-context-measurement/1 ")).toBe(true)
    expect(/Rate|canary|HINTCANARY|\.go/.test(parts[1].text)).toBe(false) // metadata only
    const sid = "ses_canary_ctx"
    const tool = "githint_hint_context"
    const ev = (id: string, t: string, content: any[]) => [
      { hook: "execute.before", event: { tool: t, sessionID: sid, id, input: { seed: "Rate" } } },
      { hook: "execute.after", event: { tool: t, sessionID: sid, id, input: { seed: "Rate" }, status: "completed", result: { content, metadata: {} } } },
    ]
    const bad = parts[1].text.replace('"result":"delivered"', '"result":"HINTCANARY_BAD"')
    const recs = [
      ...ev("call_canary_ctx1", tool, parts),
      // the marker inside ordinary file output, or under another tool name, is no measurement
      ...ev("call_canary_ctx2", "read", [{ type: "text", text: parts[1].text }]),
      ...ev("call_canary_ctx3", "evil_hint_context", parts),
      // a damaged marker: missing context coverage, never an invented measurement
      ...ev("call_canary_ctx4", tool, [parts[0], { type: "text", text: bad }]),
    ]
    const dir = mkdtempSync(join(tmpdir(), "hint-ctx-"))
    const cap = join(dir, "pipe.jsonl")
    const fake = join(dir, "capture.sh")
    writeFileSync(fake, `#!/bin/sh\ncat > "${cap}"\n`)
    chmodSync(fake, 0o755)
    try {
      await replay(recs, PROJECT, fake)
      await Bun.sleep(100)
      const pipe = readFileSync(cap, "utf8")
      expect(/func Rate|HINTCANARY_RATE_COMMENT|"ref":"hc1/.test(pipe)).toBe(false) // no source or refs leave the plugin
      const sent = pipe.split("\n").filter((l) => l.includes("execute.after")).map((l) => JSON.parse(l))
      const byId = (id: string) => sent.find((e) => e.input.callID === id).output
      const total = parts.reduce((n: number, p: any) => n + new TextEncoder().encode(p.text).length, 0)
      expect(byId("call_canary_ctx1").output).toBe(`[redacted:len=${total}]`) // every text part the hook saw
      expect(byId("call_canary_ctx1").context.context.stage).toBe("raw_tool_output")
      expect(byId("call_canary_ctx2").context).toBeUndefined()
      expect(byId("call_canary_ctx3").context).toBeUndefined()
      expect(byId("call_canary_ctx4").context_invalid).toBe(true)
    } finally {
      rmSync(dir, { recursive: true, force: true })
    }
    // through the real child: two stages of one call, and a missing context for the damaged one
    const before = spool(PROJECT).length
    const { st } = await replay(recs, PROJECT, BIN)
    expect(st.hookErrors).toBe(0)
    const obs = spool(PROJECT).slice(before)
    const ctx = obs.filter((o) => o.context)
    expect(ctx.map((o) => o.context.stage).sort()).toEqual(["delivered_context", "raw_tool_output"])
    expect(new Set(ctx.map((o) => o.correlation.tool_call_id)).size).toBe(1)
    const total = parts.reduce((n: number, p: any) => n + new TextEncoder().encode(p.text).length, 0)
    expect(ctx.find((o) => o.context.stage === "delivered_context").context.bytes).toBe(total)
    expect(ctx.find((o) => o.context.stage === "raw_tool_output").context.bytes).toBeGreaterThan(total) // structured + text as sent
    for (const o of ctx) expect(o.op).toBe("DISCOVER")
    expect(obs.filter((o) => (o.missing ?? []).includes("context")).length).toBe(1)
    expect(CANARY.test(JSON.stringify(obs))).toBe(false)
  })

  test("hook latency for 1000 events: p95 under 5 ms", async () => {
    const one = events(PROJECT)
    const recs: any[] = []
    for (let i = 0; i < 500; i++)
      for (const r of [one[6], one[7]]) recs.push({ hook: r.hook, event: { ...r.event, id: `call_canary_lat_${i}` } })
    const { lat, st } = await replay(recs, PROJECT, BIN)
    lat.sort((a, b) => a - b)
    const p50 = lat[Math.floor(lat.length * 0.5)], p95 = lat[Math.floor(lat.length * 0.95)]
    console.log(`hook latency ms: p50 ${p50.toFixed(4)} p95 ${p95.toFixed(4)} n ${lat.length} dropped ${st.dropped}`)
    expect(p95).toBeLessThan(5)
    expect(st.dropped).toBe(0)
  })
})
