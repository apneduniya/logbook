# Logbook: PRD (v1, 20 Nov 2026)

Logbook is a passive time tracker for teenagers who build projects, with or without AI. It counts **hours of productive struggle**, not hours at a keyboard, and it gives every submitted hour evidence that an outside reviewer can check.

## Why

Hack Club needs a trustworthy way to count the hours teenagers put into projects when much of the code is written with AI.

- In Stardance, 123,400 Hackatime hours were submitted and only about 27,600 could be approved after hand review.
- Most of the gap wasn't fraud:
  - hours where the person wasn't pushing themselves, deflated by hand;
  - AI use without a declaration.
- Review is manual today. Reviewers rebuild each submission from commits, heartbeats and logs, and every program writes its own idle detection.
- The YSWS handbook caps "AI coding" at one third of hours unless there's significant evidence, and no tool produces that evidence today.

Hack Club has offered a $10,000 bounty for a tracker that solves this. Its requirements are the spec for V1 (section "Requirements").

## Who

- **Teenagers** who build projects. Most of them use a Windows laptop around 10 years old. The first users are the Logbook team and the participants of one pilot program.
- **Program authors and reviewers** pick the tracker their participants use and approve hours. They're the people who decide whether Logbook gets adopted.
- **Fraud Squad and HQ** audit hours after programs approve them.

## Goals (V1, measurable)

1. Meet all 11 bounty requirements (see the table below).
2. A program can switch from Hackatime by changing a base URL, or with a patch under 50 lines.
3. Reviewers can judge a submission from a single review link, without opening any other tool.
4. On a 10-year-old Windows laptop, the agent stays under 30 MB of RAM and under 1% CPU on average.
5. Hosting stays under $500/month at Hackatime's current load (15,488 MAU, 109,759 h/month), shown by a load test.

Long term, the bounty is won when Logbook has tracked 1,000 weighted projects in the Unified YSWS DB.

## Non-goals (V1)

Hardware tracking, timelapse or screenshots, webcam, journaling or voice notes, leaderboards, Slack integration, mobile apps.

Recording keystroke content, prompts or code on the server is ruled out entirely, not just postponed.

## Productive struggle: the V1 rule

The rule follows the announcement:
- Time at a computer isn't automatically productive struggle. Repeating the same thing, or not thinking or working hard, doesn't count.
- Beginners are never penalised.
- The same rules apply to AI work and to hand-coding.

Logbook doesn't try to detect struggle directly. It **starts from active time and removes what is clearly not struggle**, giving the reason for every cut:

| Removed | Signal |
|---|---|
| Idle | No input and no agent running |
| Agent working alone | An agent is busy while the person has been away for more than N minutes |
| Rubber-stamping | Agent changes accepted with no reading or editing time between them |
| Loops | The same or a near-identical prompt or error, over and over |
| Scripted input | Input timing too regular for a human, heartbeat bursts, impossible overlaps |
| Background editor time | The editor is open but there are no edits, no navigation and no terminal use |

What remains is **credited** time. The rules are public, versioned and covered by golden tests.

## Core flows

1. **Install and sign in.**
   - A one-line installer, or a download for Windows, Mac or Linux.
   - The user signs in with Hack Club.
   - The agent finds the editors and AI tools on the machine and connects to them.
   - Hackatime history is imported.
2. **Work.** Nothing to start or stop. The dashboard updates live.
3. **Self-improvement.** Focus time, streaks, the split between you and the agent, a weekly recap, and personal projects.
4. **Submit.**
   - The user picks a project and a date range.
   - The agent builds an evidence bundle that reveals only the rows needed to re-check those minutes (ADR 0019), lists what it includes, and uploads it once the user confirms.
   - The server re-runs the rules on it (ADR 0020). The program receives a review link, and its hours API returns the credited hours.
5. **Review.** A reviewer opens the link and sees:
   - credited vs raw hours, with each cut explained;
   - a timeline of you / agent / idle;
   - commits;
   - AI sessions;
   - flags.

   They can accept or adjust, and that decision is recorded.
6. **Program integration.** Programs use the same OAuth flow and stats calls they use with Hackatime today.

## Requirements

### Must (the bounty's requirements)

| # | Requirement | V1 |
|---|---|---|
| 1 | AGPLv3 | Public repo under AGPLv3 |
| 2 | Accurately tracks productive struggle with AI | The struggle engine plus AI harness plugins |
| 3 | Motivating in its own right | Personal dashboard and weekly recap; counts personal projects too |
| 4 | Passive | Agent starts at login. Editor heartbeats work as with WakaTime. Multi-project switching handled per minute |
| 5 | Server, accounts, public profiles, projects, git, live dashboard | Go server. Hack Club sign-in. Profiles are public by default. GitHub-linked projects. Live updates over SSE |
| 6 | Privacy friendly | Only timings and counts. Prompts, code and transcripts stay on the device unless the user submits them |
| 7 | All models and harnesses | A plugin interface, with Claude Code, Codex, Gemini CLI, OpenCode, Cursor and Copilot (VS Code) in V1. Every WakaTime plugin works unchanged |
| 8 | Windows, Mac and Linux, on low-spec machines | One static Go binary per OS |
| 9 | Trustworthy | Conservative credited hours. A daily, signed evidence hash chain. Hackatime trust levels carried over |
| 10 | Auditable by an external party | Review packets, plus an admin API for Fraud Squad |
| 11 | Hosting ≤ $500/month at Hackatime's scale | Go + Postgres with per-minute rollups, proven by a load test |

### Should

- Every heartbeat is mirrored to Hackatime during the transition, so existing program tools keep working.
- Hackatime-compatible program API (OAuth `profile read`, `authenticated/me`, `authenticated/projects`, `users/:id/stats`, `trust_factor`).
- Users can reassign mis-attributed time to the right project, and every change goes into an audit log.

### Later

Hardware, opt-in timelapse, journaling, leaderboards, Slack status.

## Success metrics

- **V1 complete:** every Must row has a passing test or a recorded demo.
- **Adoption:** pilot program live, then the number of programs and weighted projects in the Unified DB, tracked weekly.
- **Trust:** for the pilot, the share of submitted hours that reviewers approve unchanged. The target is higher than Hackatime's for the same program.

## Risks and the riskiest assumption

- **Riskiest assumption: programs will accept Logbook's credited hours and review links.**
  - Cheapest test: show the review packet mock to two reviewers before building step 5.
- **Counting rules.** We need Zach to confirm that hours read from Logbook, and mirrored to Hackatime, count toward the 1,000 projects. Ketan is asking.
- **Cut rules too harsh or too soft.** Tune them on real sessions; every cut is visible and can be appealed.
- **Windows on old hardware.** Test there from the moment the agent exists.
- **Identity and bans.** Rely on Hackatime trust levels until HQ agrees to something else.

## Open decisions

- Production hosting: AWS credits for the pilot, then whatever HQ prefers.
- Final product name and domain. "Logbook" is the working name; dev runs on a boogles.dev subdomain.
