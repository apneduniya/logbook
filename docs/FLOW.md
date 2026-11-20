# AI time tracking: how it works (draft for program authors)

Working name: TBD. Draft v0.1, 19 Nov 2026.

## Normal coding time stays exactly as it is

Most programs don't accept AI-assisted code yet, so the tool must track ordinary coding exactly as Hackatime does today:

- Your existing Hackatime editor plugins keep working. The local app sits between them and Hackatime and passes every heartbeat on unchanged.
- If the app isn't running, the plugins queue heartbeats and send them later, so no time is lost.
- AI time is added on top, as its own category. Programs that don't accept AI see it clearly labelled, and that labelling also gives them an automatic AI declaration.

## The idea in one line

Track the time the **person** spends working with AI (prompting, reading what the agent did, reviewing diffs, testing, fixing), not the time the agent spends running. Keep everything private on the laptop until the person submits hours, then give reviewers evidence they can check.

## For a Hack Clubber

1. **Install once.** A small app for Windows, Mac and Linux that runs fine on an old laptop. Sign in with your Hack Club / Hackatime account. It finds the AI tools you use (Claude Code, Codex, Cursor, Copilot and others) and connects to them automatically.
2. **Work as normal.** Nothing to start or stop. The app notes, on your own computer:
   - when you are active (how often you type or click, not what you type)
   - which project you're working in
   - what your AI tool is doing: prompt sent, agent running, agent finished, changes accepted or rejected
   - your git commits
3. **Every minute gets a label:**
   - **You:** writing prompts, reading output, reviewing changes, editing, running and testing
   - **Agent only:** the agent is working while you're away
   - **Idle**

   Only "You" minutes count. Minutes that clearly aren't struggle are taken off, such as accepting every change without looking, sending the same prompt again and again, or long stretches with no input.
4. **See your progress live.** A dashboard shows your hours per project, how your time splits between you and the agent, and your streaks, plus a weekly summary of what you worked on. Hours also go to Hackatime, so they show up where programs already look.
5. **Submit to a program.** Pick the project and the dates. The app builds an evidence bundle. You look through it, then upload it. Nothing else ever leaves your laptop.

## For a program and its reviewers

- **Setup:** add the tool to your list of accepted trackers. The submission form takes the project's link from the tool.
- **What a reviewer sees for each submitted hour:**
  - a minute-by-minute timeline of your time and agent time
  - what changed in git
  - any flags, such as long agent-only stretches, possible automated sessions or repeated prompts
- **Why the evidence can be trusted:** each day the app sends the server a fingerprint (hash) of that day's local evidence. If someone edits the evidence later, it won't match the fingerprint.
- **Audits:** a reviewer can ask for the full evidence bundle, which can include optional low-rate timelapse frames. If the person won't send it, the program can reject the hours.

## Privacy

| Data | Stays on the laptop | Sent to the server |
|---|---|---|
| Prompts, agent transcripts, code | Always | Never (only as part of an evidence bundle the person chooses to submit) |
| Keystroke content | Never recorded | Never |
| Activity timings and counts | Yes | Credited minutes per project |
| Timelapse frames (optional, off by default) | Yes | Only for submitted hours, after the person reviews them |
| Evidence fingerprint | Yes | Yes (the hash only) |

No webcam, no microphone, and no screenshots unless the person turns them on.

## Fairness

- Credit is the same whatever AI model or subscription you use. There's no reward for using more tokens.
- Beginners get full credit for real struggle, even on simple projects.

## The proof of concept (this week)

- The local app, for Claude Code and Codex: their hooks and telemetry, combined with activity and git, turned into "You" / "Agent only" / "Idle" minutes.
- A live dashboard with per-project hours and the split between you and the agent.
- Hours sent to Hackatime.
- A daily evidence fingerprint and a basic reviewer timeline.
