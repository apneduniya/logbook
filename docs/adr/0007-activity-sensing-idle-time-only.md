# 0007: Sense activity through OS idle time and window focus, never keystroke hooks

- Status: accepted
- Date: 2026-11-20

## Context

To tell "you" from "idle", the agent has to know whether a person is at the machine. Keyloggers and screenshots would put off privacy-conscious users, could be flagged by antivirus, and have a large attack surface.

## Decision

The agent samples, about every 5 seconds:
- the OS idle time (seconds since the last input):
  - Windows: `GetLastInputInfo`;
  - macOS: `CGEventSourceSecondsSinceLastEventType`;
  - Linux: X11 XScreenSaver / Wayland `ext-idle-notify` / logind `IdleHint`;
- the process name of the focused window (an editor, terminal, browser, AI app or other). Window titles aren't stored.

These are the only activity signals in V1. Together with editor heartbeats, AI harness events and git events, they're what the struggle engine reads.

## Consequences

- No accessibility permission or input monitoring is needed on macOS for idle time. The focused app name may still need permissions on macOS; this needs testing.
- Input rhythm can't be analysed to detect scripted input, so V1 detects scripted activity from heartbeat and event patterns instead.
