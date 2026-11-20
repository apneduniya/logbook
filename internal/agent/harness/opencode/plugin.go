package opencode

import (
	"encoding/json"
	"strings"
)

// marker identifies a plugin file Logbook wrote. Install and Uninstall only
// ever replace or remove a file that carries it.
const marker = "logbook-managed-plugin"

// binPlaceholder is replaced by the JSON-encoded binary path.
const binPlaceholder = "__LOGBOOK_BIN__"

// pluginSource is the OpenCode plugin Logbook installs. It works with
// OpenCode V2 (default export with id and setup) and V1 1.18.29+ (the same
// object's server function). It sends metadata only: session id, the
// session's directory (V2: from session.created; V1: the plugin's), model id, tool name, prompt length in
// characters and a permission answer. Prompt text, tool arguments and output
// never leave the plugin. Each event runs `logbook hook opencode <event>` in
// the background with the metadata as JSON on stdin; nothing waits for it.
const pluginSource = `// ` + marker + `
// Logbook plugin for OpenCode. Written by "logbook install" and removed by
// "logbook uninstall"; edits are overwritten on the next install.
// It reports metadata only (session id, project directory, model id, tool
// name, prompt length, permission answer) to the local Logbook agent. Prompt
// text, tool arguments and tool output never leave this file.
import { spawn } from "node:child_process"

const BIN = ` + binPlaceholder + `
const models = new Map()

function chars(text) {
  return typeof text === "string" ? Array.from(text).length : 0
}

function str(v) {
  return typeof v === "string" ? v : ""
}

function send(event, meta) {
  try {
    const child = spawn(BIN, ["hook", "opencode", event], {
      stdio: ["pipe", "ignore", "ignore"],
      windowsHide: true,
    })
    const timer = setTimeout(() => {
      try { child.kill() } catch {}
    }, 5000)
    if (timer.unref) timer.unref()
    child.on("error", () => clearTimeout(timer))
    child.on("exit", () => clearTimeout(timer))
    child.stdin.on("error", () => {})
    child.stdin.end(JSON.stringify(meta))
    if (child.unref) child.unref()
  } catch {}
}

function meta(sessionID, dir, extra) {
  const id = str(sessionID)
  return Object.assign({ session_id: id, cwd: str(dir), model: str(models.get(id)) }, extra)
}

function decision(reply) {
  if (reply === "reject") return "deny"
  if (reply === "once" || reply === "always") return "allow"
  return ""
}

function safe(fn) {
  return (...args) => {
    try { fn(...args) } catch {}
  }
}

async function setup(ctx) {
  const dir = () => str(ctx && ctx.location && ctx.location.directory)
  // ctx.event.subscribe streams every event on the server, including
  // sessions in other directories that another plugin instance reports.
  // Only sessions this instance owns are reported: ones created in its
  // location, or ones its own (location-scoped) hooks have seen.
  const owned = new Map()
  const own = (id) => {
    if (id && !owned.has(id)) owned.set(id, dir())
    return owned.get(id) || dir()
  }
  await ctx.session.hook("prompt", safe((ev) => {
    const id = str(ev.sessionID)
    send("prompt", meta(id, own(id), { prompt_chars: chars(ev.prompt && ev.prompt.text) }))
  }))
  await ctx.tool.hook("execute.before", safe((ev) => {
    const id = str(ev.sessionID)
    send("tool-start", meta(id, own(id), { tool: str(ev.tool) }))
  }))
  await ctx.tool.hook("execute.after", safe((ev) => {
    const id = str(ev.sessionID)
    send("tool-end", meta(id, own(id), { tool: str(ev.tool) }))
  }))
  const controller = new AbortController()
  const types = [
    "session.created", "session.model.selected", "session.deleted",
    "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted",
    "permission.asked", "permission.replied",
  ]
  const onEvent = safe((e) => {
    const d = (e && e.data) || {}
    const id = str(d.sessionID)
    if (!id) return
    if (e.type === "session.created") {
      const where = str(d.location && d.location.directory) || dir()
      if (where !== dir()) return
      owned.set(id, where)
      if (d.model && d.model.id) models.set(id, str(d.model.id))
      send("session-start", meta(id, where, {}))
      return
    }
    if (!owned.has(id)) return
    const where = owned.get(id)
    switch (e.type) {
      case "session.model.selected":
        if (d.model && d.model.id) models.set(id, str(d.model.id))
        break
      case "session.deleted":
        send("session-end", meta(id, where, {}))
        models.delete(id)
        owned.delete(id)
        break
      case "session.execution.succeeded":
      case "session.execution.failed":
      case "session.execution.interrupted":
        send("turn-end", meta(id, where, {}))
        break
      case "permission.asked":
        send("permission-ask", meta(id, where, {}))
        break
      case "permission.replied":
        send("permission-reply", meta(id, where, { decision: decision(d.reply) }))
        break
    }
  })
  ;(async () => {
    for await (const e of ctx.event.subscribe({ types, signal: controller.signal })) onEvent(e)
  })().catch(() => {})
  return () => controller.abort()
}

async function server(input) {
  const dir = str(input && input.directory)
  return {
    event: async ({ event }) => {
      safe(() => {
        const p = (event && event.properties) || {}
        const info = p.info || {}
        switch (event.type) {
          case "session.created":
            send("session-start", meta(info.id, dir, {}))
            break
          case "session.deleted":
            send("session-end", meta(info.id, dir, {}))
            models.delete(str(info.id))
            break
          case "session.idle":
            send("turn-end", meta(p.sessionID, dir, {}))
            break
          case "permission.asked":
          case "permission.updated":
            send("permission-ask", meta(p.sessionID, dir, {}))
            break
          case "permission.replied":
            send("permission-reply", meta(p.sessionID, dir, { decision: decision(p.response || p.reply) }))
            break
        }
      })()
    },
    "chat.message": async (inp, out) => {
      safe(() => {
        if (inp && inp.model && inp.model.modelID) models.set(str(inp.sessionID), str(inp.model.modelID))
        const parts = (out && Array.isArray(out.parts)) ? out.parts : []
        let n = 0
        for (const part of parts) if (part && part.type === "text" && !part.synthetic) n += chars(part.text)
        send("prompt", meta(inp && inp.sessionID, dir, { prompt_chars: n }))
      })()
    },
    "tool.execute.before": async (inp) => {
      safe(() => send("tool-start", meta(inp && inp.sessionID, dir, { tool: str(inp && inp.tool) })))()
    },
    "tool.execute.after": async (inp) => {
      safe(() => send("tool-end", meta(inp && inp.sessionID, dir, { tool: str(inp && inp.tool) })))()
    },
  }
}

export default { id: "logbook", setup, server }
`

// renderPlugin returns the plugin source for binPath.
func renderPlugin(binPath string) []byte {
	b, _ := json.Marshal(binPath) // marshalling a string can't fail
	return []byte(strings.Replace(pluginSource, binPlaceholder, string(b), 1))
}
