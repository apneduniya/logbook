// Drives the Logbook OpenCode plugin with fake V2 and V1 contexts. Every
// fake payload carries private text (the sentinel) that must never reach the
// hook binary. Usage: node driver.mjs <plugin.mjs> <sentinel>
const [, , pluginPath, S] = process.argv
const plugin = (await import(pluginPath)).default

const hooks = {}
const events = [
  { type: "session.created", data: { sessionID: "ses_v2", location: { directory: "/home/teen/my-app" }, model: { id: "claude-sonnet-5-5", providerID: "anthropic" }, title: S } },
  // A session in another directory on the same server: another plugin
  // instance reports it, so this one must stay quiet.
  { type: "session.created", data: { sessionID: "ses_other", location: { directory: "/home/teen/other-app" }, title: S } },
  { type: "permission.asked", data: { sessionID: "ses_other", action: "bash", resources: [S] } },
  { type: "session.execution.succeeded", data: { sessionID: "ses_other" } },
  { type: "permission.asked", data: { sessionID: "ses_v2", action: "bash", resources: [S] } },
  { type: "permission.replied", data: { sessionID: "ses_v2", requestID: "per_1", reply: "reject" } },
  { type: "session.execution.succeeded", data: { sessionID: "ses_v2" } },
  { type: "session.text.delta", data: { sessionID: "ses_v2", delta: S } },
]
let finish
const finished = new Promise((r) => { finish = r })
const ctx = {
  location: { directory: "/home/teen/my-app" },
  session: { hook: async (name, fn) => { hooks["session." + name] = fn; return { dispose() {} } } },
  tool: { hook: async (name, fn) => { hooks["tool." + name] = fn; return { dispose() {} } } },
  event: {
    subscribe: ({ types }) => (async function* () {
      for (const e of events) if (types.includes(e.type)) yield e
      await finished
      // ses_resumed was created before the plugin loaded; the prompt hook
      // (scoped to this location) made it ours. ses_unknown never was.
      yield { type: "session.execution.succeeded", data: { sessionID: "ses_resumed" } }
      yield { type: "session.execution.succeeded", data: { sessionID: "ses_unknown" } }
      yield { type: "session.deleted", data: { sessionID: "ses_other" } }
      yield { type: "session.deleted", data: { sessionID: "ses_v2" } }
    })(),
  },
}

const cleanup = await plugin.setup(ctx)
await new Promise((r) => setTimeout(r, 50))
hooks["session.prompt"]({ sessionID: "ses_v2", prompt: { text: "héllo " + S, files: [{ uri: "file:///" + S }] } })
hooks["session.prompt"]({ sessionID: "ses_resumed", prompt: { text: "ok" } })
hooks["tool.execute.before"]({ sessionID: "ses_v2", tool: "bash", input: { command: S } })
hooks["tool.execute.after"]({ sessionID: "ses_v2", tool: "bash", input: { command: S }, status: "completed", result: S })
finish()
await new Promise((r) => setTimeout(r, 50))
if (typeof cleanup === "function") cleanup()

// OpenCode V1 calls server() and uses the returned hooks.
const v1 = await plugin.server({ directory: "/home/teen/v1-app", project: { id: S } })
await v1.event({ event: { type: "session.created", properties: { info: { id: "ses_v1", title: S } } } })
await v1["chat.message"](
  { sessionID: "ses_v1", model: { providerID: "openai", modelID: "gpt-5.5" } },
  { message: { id: "m1" }, parts: [{ type: "text", text: "abc " + S }, { type: "file", url: S }] },
)
await v1["tool.execute.before"]({ tool: "read", sessionID: "ses_v1", callID: "c1" }, { args: { filePath: S } })
await v1.event({ event: { type: "session.idle", properties: { sessionID: "ses_v1" } } })

// Let the spawned hook processes finish writing.
await new Promise((r) => setTimeout(r, 1500))
