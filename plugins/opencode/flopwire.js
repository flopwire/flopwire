// Flopwire for opencode (https://github.com/flopwire/flopwire, issue #62).
// `flopwire setup` installs this file into opencode's global plugin
// directory and `flopwire setup --remove` deletes it; do not edit the
// installed copy. It needs the flopwire binary on PATH and the device agent
// (flopwire agent run).
//
// What it does, in every opencode process that loads it:
//
//   - Delivers messages for a session. After each tool call
//     (tool.execute.after) it stores them with client.session.promptAsync
//     and noReply, which adds a user message without starting a turn.
//     When the user sends a prompt (chat.message) it adds them to that
//     prompt as more text parts. One part per message: the message's
//     wrapper, with metadata {"flopwire": {"id"}} naming the same message
//     (the device agent's read receipts check that the two agree).
//     `flopwire hook` leases the messages; the plugin confirms them only
//     once opencode reports their parts stored (message.part.updated; not
//     on promptAsync's answer), so a failed delivery is offered again
//     rather than lost. Never into a subagent's session, and never with
//     promptAsync without noReply, which would wake an idle session.
//   - Puts Flopwire's standing instruction and tool guidance into every
//     model call's system prompt (experimental.chat.system.transform).
//   - Serves Flopwire's tools (grep, search, sessions, read, peers, send,
//     inbox) as plugin tools: each call runs `flopwire mcp --call` as the
//     calling session (a subagent's calls run as its parent session).
//   - Keeps <flopwire config dir>/opencode/<pid>.json naming this
//     process's top-level sessions, for presence, and deletes the files of
//     opencode processes that are gone.
//   - Tells the device agent when a turn starts and ends, and sets
//     FLOPWIRE_SESSION_ID and FLOPWIRE_AGENT for shell commands.
//
// FLOPWIRE_BIN (the flopwire binary, skipping the search),
// FLOPWIRE_HOOK_ARGS (a JSON array replacing ["hook"]) and FLOPWIRE_SOCKET
// (the agent's control socket) exist for `flopwire probe`.

import { tool } from "@opencode-ai/plugin"
import { spawn } from "node:child_process"
import { accessSync, appendFileSync, constants, mkdirSync, readdirSync, readFileSync, renameSync, statSync, unlinkSync, writeFileSync } from "node:fs"
import { homedir } from "node:os"
import { delimiter, dirname, isAbsolute, join } from "node:path"

// findBin finds the flopwire binary: {bin, via} with via env, recorded, path
// or known (FLOPWIRE_HOOK_VIA for the binary), or null after one line on
// stderr naming what it searched and the fix.
function findBin(env = process.env, platform = process.platform) {
  if (env.FLOPWIRE_BIN) return { bin: env.FLOPWIRE_BIN, via: "env" }
  const home = env.HOME || homedir()
  let dir
  if (env.FLOPWIRE_CONFIG) dir = dirname(env.FLOPWIRE_CONFIG)
  else if (platform === "darwin") dir = join(home, "Library", "Application Support", "flopwire")
  else dir = env.XDG_CONFIG_HOME && isAbsolute(env.XDG_CONFIG_HOME) ? join(env.XDG_CONFIG_HOME, "flopwire") : join(home, ".config", "flopwire")
  const exe = (p) => {
    try { if (!statSync(p).isFile()) return false; accessSync(p, constants.X_OK); return true } catch { return false }
  }
  const file = join(dir, "binary-path")
  let recorded = ""
  try { recorded = readFileSync(file, "utf8").split("\n")[0].trim() } catch {}
  let onPath = ""
  for (const d of (env.PATH || "").split(delimiter)) {
    if (isAbsolute(d) && exe(join(d, "flopwire"))) { onPath = join(d, "flopwire"); break }
  }
  // A flopwire on PATH newer than the recorded one wins: the recorded path
  // can name an older install that still exists.
  const newer = (a, b) => { try { return statSync(a).mtimeMs > statSync(b).mtimeMs } catch { return false } }
  if (recorded && exe(recorded) && !(onPath && newer(onPath, recorded))) return { bin: recorded, via: "recorded" }
  if (onPath) return { bin: onPath, via: "path" }
  for (const d of ["/opt/homebrew/bin", "/usr/local/bin", join(home, "go", "bin"), join(home, ".local", "bin")]) {
    if (exe(join(d, "flopwire"))) return { bin: join(d, "flopwire"), via: "known" }
  }
  console.error(`flopwire (opencode plugin): no flopwire binary: the recorded path (${recorded || "none"}, from ${file}) is not executable, flopwire is not on PATH (${env.PATH || ""}), and not in /opt/homebrew/bin, /usr/local/bin, ~/go/bin or ~/.local/bin; fix: run flopwire setup`)
  return null
}
const SOCKET = process.env.FLOPWIRE_SOCKET || ""
const HOOK = (() => {
  try {
    const a = JSON.parse(process.env.FLOPWIRE_HOOK_ARGS || "")
    if (Array.isArray(a) && a.every((x) => typeof x === "string")) return a
  } catch {}
  return SOCKET ? ["hook", "--socket", SOCKET] : ["hook"]
})()
const HOOK_TIMEOUT = 5000
const TOOL_TIMEOUT = 90000

// run runs flopwire with input on stdin; it never throws. It looks for
// the binary on every run, so an upgrade that moves it takes effect
// without restarting opencode.
function run(args, input, env, timeout) {
  return new Promise((resolve) => {
    let out = "", err = "", done = false
    const finish = (code) => { if (!done) { done = true; resolve({ code, out, err }) } }
    const found = findBin()
    if (!found) return finish(-1, (err = "no flopwire binary"))
    let p
    try {
      p = spawn(found.bin, args, { env: { ...process.env, FLOPWIRE_HOOK_VIA: found.via, ...env }, stdio: ["pipe", "pipe", "pipe"] })
    } catch (e) {
      return finish(-1, (err = String(e)))
    }
    const timer = setTimeout(() => { try { p.kill("SIGKILL") } catch {} ; err += `timed out after ${timeout} ms`; finish(-1) }, timeout)
    p.stdout.on("data", (b) => { out += b })
    p.stderr.on("data", (b) => { err += b })
    p.on("error", (e) => { clearTimeout(timer); err += String(e); finish(-1) })
    p.on("close", (code) => { clearTimeout(timer); finish(code) })
    p.stdin.on("error", () => {})
    p.stdin.end(input)
  })
}

function lastJSON(text) {
  const lines = text.trim().split("\n")
  for (let i = lines.length - 1; i >= 0; i--) {
    try { return JSON.parse(lines[i]) } catch {}
  }
  return null
}

// ascendingID makes an opencode ascending id ("prt", "msg"): 12 hex digits
// of (milliseconds * 4096 + a counter) and 14 random characters, as
// opencode's own ids.
let lastMS = 0, counter = 0
function ascendingID(prefix) {
  const now = Date.now()
  if (now !== lastMS) { lastMS = now; counter = 0 }
  counter++
  const v = (BigInt(now) * 4096n + BigInt(counter)) & 0xffffffffffffn
  const abc = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
  let r = ""
  for (let i = 0; i < 14; i++) r += abc[Math.floor(Math.random() * abc.length)]
  return `${prefix}_${v.toString(16).padStart(12, "0")}${r}`
}

function alive(pid) {
  try { process.kill(pid, 0); return true } catch (e) { return e && e.code === "EPERM" }
}

// zodOf turns one JSON-schema property of a Flopwire tool into zod.
function zodOf(z, p) {
  let s
  if (Array.isArray(p.enum)) s = z.enum(p.enum)
  else if (p.type === "integer") s = z.number().int()
  else if (p.type === "number") s = z.number()
  else if (p.type === "boolean") s = z.boolean()
  else if (p.type === "array") s = z.array(z.string())
  else s = z.string()
  return p.description ? s.describe(p.description) : s
}

export const Flopwire = async ({ client }) => {
  const started = Math.round(performance.timeOrigin)
  const hello = lastJSON((await run(HOOK, JSON.stringify({ hook_event_name: "Hello", harness: "opencode" }), {}, HOOK_TIMEOUT)).out)
  if (!hello || !hello.instruction) return {} // no flopwire binary, or one too old: stay out of the way

  const regDir = hello.registry || ""
  const regFile = regDir ? join(regDir, `${process.pid}.json`) : ""
  const logFile = regDir ? join(regDir, `${process.pid}.log`) : ""
  const log = (...a) => { if (logFile) try { appendFileSync(logFile, `${new Date().toISOString()} ${a.join(" ")}\n`) } catch {} }

  // Clean the files of opencode processes that are gone.
  if (regDir) {
    try {
      for (const f of readdirSync(regDir)) {
        const m = /^(\d+)\.(json|log)$/.exec(f)
        if (m && Number(m[1]) !== process.pid && !alive(Number(m[1]))) try { unlinkSync(join(regDir, f)) } catch {}
      }
    } catch {}
  }

  const parents = new Map() // session -> parent session ("" for a top-level one)
  const held = new Set() // this process's top-level sessions
  const lastUser = new Map() // session -> {agent, model} of its last user message
  const queues = new Map() // session -> the delivery running for it

  const writeRegistry = () => {
    if (!regFile) return
    try {
      if (held.size === 0) { try { unlinkSync(regFile) } catch {} ; return }
      mkdirSync(regDir, { recursive: true })
      const tmp = `${regFile}.tmp`
      writeFileSync(tmp, JSON.stringify({ pid: process.pid, started, sessions: [...held] }))
      renameSync(tmp, regFile)
    } catch (e) { log("registry:", e) }
  }
  process.on("exit", () => { if (regFile && held.size) try { unlinkSync(regFile) } catch {} })

  const noteSession = (info) => {
    if (!info || !info.id) return
    const parent = info.parentID || ""
    parents.set(info.id, parent)
    if (!parent && !held.has(info.id)) { held.add(info.id); writeRegistry() }
  }

  // parentOf is the session's parent ("" for a top-level session).
  const parentOf = async (sid) => {
    if (parents.has(sid)) return parents.get(sid)
    try {
      const r = await client.session.get({ path: { id: sid } })
      if (r && r.data) { noteSession(r.data); return parents.get(sid) || "" }
    } catch (e) { log("session.get:", e) }
    return ""
  }
  const rootOf = async (sid) => {
    let s = sid
    for (let i = 0; i < 16; i++) {
      const p = await parentOf(s)
      if (!p) return s
      s = p
    }
    return s
  }

  // hook runs `flopwire hook` for an event of session sid. A subagent's
  // event goes as its root session's, with its own id as agent_id: the
  // hook then delivers nothing.
  const hook = async (event, sid, extra) => {
    const root = await rootOf(sid)
    const input = { hook_event_name: event, harness: "opencode", session_id: root, ...extra }
    if (root !== sid) input.agent_id = sid
    else if (!held.has(sid)) { held.add(sid); writeRegistry() }
    const r = await run(HOOK, JSON.stringify(input), {}, HOOK_TIMEOUT)
    if (r.code !== 0 && r.err) log(event, "hook:", r.err.trim().slice(0, 300))
    return { root, out: lastJSON(r.out) }
  }

  // The delivery format (cmd/flopwire hook_opencode.go): one text part per
  // message, its text the message's wrapper, its metadata naming the same
  // id. The parser counts a part as delivered only when the two agree.
  const messagesOf = (out) => (Array.isArray(out.messages) ? out.messages : []).filter((m) => m && m.id && m.text)
  const partOf = (m) => ({ type: "text", text: m.text, metadata: { flopwire: { id: m.id } } })
  const confirm = (sid, ids, instruction) =>
    run(HOOK, JSON.stringify({ hook_event_name: "Confirm", harness: "opencode", session_id: sid, ids, instruction }), {}, HOOK_TIMEOUT)
  const pendingParts = new Map() // part id -> what to confirm once opencode stores it

  // deliver takes the session's pending messages and stores them in it.
  // One at a time per session. promptAsync answers before opencode stores
  // the message, and a failure after that answer (an unknown agent or
  // model) only publishes session.error: so each part gets its own id, and
  // its message is confirmed when opencode reports the part stored
  // (message.part.updated), never on promptAsync's answer.
  const deliver = (event, sid, extra) => {
    const prev = queues.get(sid) || Promise.resolve()
    const next = prev.then(async () => {
      const { root, out } = await hook(event, sid, extra)
      if (root !== sid || !out) return
      const msgs = messagesOf(out)
      if (!msgs.length) {
        if (out.instruction) await confirm(sid, [], true)
        return
      }
      const parts = msgs.map((m, i) => {
        const id = ascendingID("prt")
        pendingParts.set(id, { sid, ids: [m.id], instruction: i === 0 && !!out.instruction })
        return { id, ...partOf(m) }
      })
      const body = { noReply: true, parts }
      const u = lastUser.get(sid)
      if (u && u.agent) body.agent = u.agent
      if (u && u.model && u.model.providerID && u.model.modelID) body.model = { providerID: u.model.providerID, modelID: u.model.modelID }
      let ok = false
      try {
        const r = await client.session.promptAsync({ path: { id: sid }, body })
        ok = !(r && r.error)
        if (!ok) log("promptAsync:", JSON.stringify(r.error).slice(0, 300))
      } catch (e) { log("promptAsync:", e) }
      // Not sent: nothing will be stored, so nothing is confirmed; the
      // lease ends and the messages come again, marked.
      if (!ok) for (const p of parts) pendingParts.delete(p.id)
    }).catch((e) => log("deliver:", e))
    queues.set(sid, next)
    return next
  }

  const tools = {}
  for (const t of hello.tools || []) {
    const schema = t.inputSchema || {}
    const req = new Set(schema.required || [])
    const args = {}
    for (const [k, p] of Object.entries(schema.properties || {})) {
      const s = zodOf(tool.schema, p)
      args[k] = req.has(k) ? s : s.optional()
    }
    const name = t.name
    tools[name] = tool({
      description: t.description,
      args,
      async execute(a, ctx) {
        const root = await rootOf(ctx.sessionID)
        const cargs = ["mcp", "--call", name]
        if (SOCKET) cargs.push("--socket", SOCKET)
        const r = await run(cargs, JSON.stringify(a || {}), { FLOPWIRE_SESSION_ID: root, FLOPWIRE_AGENT: "opencode" }, TOOL_TIMEOUT)
        const text = r.out.trim() || r.err.trim()
        if (r.code !== 0) throw new Error(text || `flopwire mcp --call ${name} failed`)
        return text
      },
    })
  }

  const system = [hello.instruction, hello.mcp_instructions].filter(Boolean)

  return {
    event: async ({ event }) => {
      const p = event.properties || {}
      switch (event.type) {
        case "session.created":
        case "session.updated":
          noteSession(p.info)
          break
        case "session.deleted":
          if (p.info && p.info.id && held.delete(p.info.id)) {
            writeRegistry()
            await hook("SessionEnd", p.info.id, {})
          }
          break
        case "session.idle":
          if (p.sessionID) await hook("Stop", p.sessionID, {})
          break
        case "message.part.updated": {
          const part = p.part
          const c = part && pendingParts.get(part.id)
          if (c) { pendingParts.delete(part.id); await confirm(c.sid, c.ids, c.instruction) }
          break
        }
        case "message.updated":
          if (p.info && p.info.role === "user" && p.info.sessionID) lastUser.set(p.info.sessionID, { agent: p.info.agent, model: p.info.model })
          break
      }
    },
    "chat.message": async (input, output) => {
      if (!input || !input.sessionID) return
      const sid = input.sessionID
      lastUser.set(sid, { agent: input.agent, model: input.model })
      const msg = output && output.message
      if (!msg || !msg.id || !Array.isArray(output.parts)) return
      // A prompt is being submitted: the messages go into it as one more
      // text part, which the turn it starts reads (a separate message
      // stored now would come after the turn's first model call and earn a
      // second reply). They are confirmed when opencode stores the part.
      const { root, out } = await hook("UserPromptSubmit", sid, {})
      if (root !== sid || !out) return
      const msgs = messagesOf(out)
      if (!msgs.length) {
        if (out.instruction) await confirm(sid, [], true)
        return
      }
      msgs.forEach((m, i) => {
        const id = ascendingID("prt")
        pendingParts.set(id, { sid, ids: [m.id], instruction: i === 0 && !!out.instruction })
        output.parts.push({ id, sessionID: sid, messageID: msg.id, ...partOf(m) })
      })
    },
    "tool.execute.before": async (input) => {
      if (input && input.sessionID) await hook("PreToolUse", input.sessionID, { tool_name: input.tool, tool_use_id: input.callID })
    },
    "tool.execute.after": async (input) => {
      if (input && input.sessionID) await deliver("PostToolUse", input.sessionID, { tool_name: input.tool, tool_use_id: input.callID })
    },
    "experimental.chat.system.transform": async (_input, output) => {
      if (output && Array.isArray(output.system)) output.system.push(...system)
    },
    "shell.env": async (input, output) => {
      if (!input || !input.sessionID || !output || !output.env) return
      output.env.FLOPWIRE_SESSION_ID = await rootOf(input.sessionID)
      output.env.FLOPWIRE_AGENT = "opencode"
    },
    tool: tools,
  }
}
