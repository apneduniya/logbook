# Logbook web: design rules

The approved direction is **B3, mono HUD**, on the Doop canvas _Logbook web UI_. Data sits in a strict grid of hairline panels, like a ship's log read on an instrument panel. These rules say how to choose; they are not a checklist to apply everywhere.

## References (why this direction)

| Reference                                        | What we took                                                                      |
| ------------------------------------------------ | --------------------------------------------------------------------------------- |
| parkerhendo, "the correct aesthetic of software" | Dense data in a strict hairline grid, read like a terminal or HUD                 |
| mkdir.directory                                  | Ranked lists as big rows on hairlines, counts on the right                        |
| Maritime, Detent UI                              | Generous framing, near-black dark mode (never navy)                               |
| Departure Mono (Helena Zhang)                    | One pixel voice for figures and panel heads                                       |
| Hackatime's dashboard (what teens know)          | Today, streak, week, projects and languages are the questions people ask          |
| Invoice layouts                                  | The review page as one document: meta block, totals, itemised lines               |
| Sentry's issue page                              | Figures at the head, the body on the left, a rail of actions on the right         |
| GitHub Actions run summary and run list          | A fact strip of figures; list rows with a status glyph, not a pill                |
| GitHub's Verified badge                          | The evidence line's small bordered `VERIFIED` tag                                 |
| Harvest and Toggl Track approvals                | One total with its split; the decision as "APPROVED by …" plus a comment          |
| Vercel's deployment summary                      | One row per day with its numbers right-aligned                                    |
| Vercel's deployment status filter                | Filter segments carry their counts (`READY 2`)                                    |
| Reticket's event list                            | A mono dateline over the name, the amount on the right                            |
| GitHub's authorized OAuth apps                   | Apps with access: a count sentence, then one row per app with Revoke              |
| Linear's OAuth app page and new-app form         | An app as a record: facts apart from credentials; a form the size of the API body |
| GitHub's and Linear's consent screens            | One framed sheet: who asks, what each scope allows, where you go back to          |
| Google's account-linking consent                 | Scope rows in plain words; "remove this access any time"                          |
| Discord's install settings                       | Each scope with a one-line explanation                                            |

## Tokens

The tokens live in `src/routes/layout.css` and are copied from the canvas.

- **Ground and ink:** ledger paper `#F1EFE8` and iron-gall ink `#17160F` in light; `#0E0E0D` and `#ECEAE2` in dark. The theme follows the system setting.
- **The one accent:** ultramarine, `#2740D6` in light and `#7387FF` in dark. It marks **live state only**: "tracking now", the live project's edge, today's cell, and a submission's verifying spinner. It is never decoration, links, buttons or a verdict: decisions are carried by ink inversion (`DecisionStamp`).
- **Rules:** `--color-rule` hairlines. There are no shadows and no radii on panels.
- **Agent alone:** a hatched fill (`--hatch`). Idle is `--color-fill`.

## Type: three voices, each with a job

| Voice | Font           | Used for                                                                                                                 |
| ----- | -------------- | ------------------------------------------------------------------------------------------------------------------------ |
| Pixel | Departure Mono | Big figures (today, streak, totals), panel heads in caps, the wordmark, empty-state lines like `( NO COUNTED TIME YET )` |
| Sans  | Geist          | Everything people read: names, sentences, buttons, form labels                                                           |
| Mono  | Geist Mono     | Numbers in rows, dates, durations, small labels (`.lbl`)                                                                 |

Don't make the whole UI pixel or mono; hierarchy comes from the contrast between voices.

- **Sentences are sans,** even small help text. Use `.hint`: Geist at 13 px, `ink-2`, 60 characters wide at most. The mono `.lbl` voice is for real labels and meta, such as counts, dates, key prefixes and legends; a full sentence in tiny spaced mono is the hardest text on the page to read.

## Layout

- **Desktop:** panels sit on an 8/4 grid (`calc((100% - 12px) * 8 / 12)`) with 12 px gaps. Panel heads are 36 px tall. Bodies are padded 18 × 16.
- **Edges:** every element starts on its panel's left edge. Nothing bleeds past a column, including hover tints, which stay inside the element.
  - Rows in an unpadded list start 14 px in, on the head's line; padded bodies are 16 px in.
  - The root font size is 13 px, so Tailwind's spacing steps are 3.25 px (`px-4` is 13 px). Write a panel edge's padding in pixels (`px-[14px]`), never as a spacing step.
- **Phones (under 900 px):** panels stack. Under 720 px the tabs wrap to their own row, and Profile steps aside unless it is open (the account menu has it), so the row fits a 375 px screen. Under 480 px project rows drop the keyboard column.

## Interaction

- The active tab or segment is an **inverted block** (ink background), never an underline.
- **Links:** muted at rest and ink on hover. Arrows (→) only on links that go somewhere else.
- **Buttons:**
  - Secondary: bordered, filling with ink on hover.
  - Primary: solid ink, in two places only: "Sign in with Hack Club" and Allow on the OAuth consent page. Deny beside Allow stays secondary.
  - Destructive actions ask once inline (`ConfirmButton`) instead of opening a modal. The trigger is a bordered button, not muted text, because it acts. Its error shows by its own row, never in a page-level notice.
  - Deciding actions that need a reason (reject, adjust) use `ReasonConfirm`: the same inline ask, with a reason field (and hours for adjust) checked on submit. A failure keeps what was typed.
  - Copy buttons (`CopyField`) are small solid-ink pixel caps inside the value's box. A review link is copied, never printed in full; a one-time secret is masked until "Show".
- **Form fields** have an `ink-3` border at rest. A hairline-only field is invisible (1.3:1).
- **Errors on fields** show after the field is left or a save is tried, not from the first keystroke.
- **Disabled:** don't disable a submit button to signal invalid input; let it explain on submit. Buttons disable only while busy.
- **Motion:** 150 ms colour and transform transitions, plus the 8-step spinner of the live badge. Everything stops under `prefers-reduced-motion`.

## Charts

- A chart whose numbers appear only on hover also carries a visually hidden table, so screen readers get the numbers.
- Copy never promises an interaction the device lacks: hover hints are hidden on touch screens.
- On phones the year grid scrolls with fixed columns, and its window is sized to whole columns so the edge never cuts a dot in half.
- **Minute strips** (`MinuteStrip`) use the review packet's alphabet: counted is ink, agent alone hatched, cut and idle fill, unbacked outlined, and a mismatch is ink with a notch (a shorter bar). Up to two hours draw a cell per minute; longer windows draw runs. Commits are ticks above the strip whose detail line shows under it on hover, focus or tap. Every strip has a hidden table of its runs.

## Review page

- An audited timesheet read like an invoice: the SUBMISSION meta block, three figures with counted the biggest, a one-sentence evidence line with its tag, then itemised panels.
- On desktop it uses the 8/4 grid, with a rail of TRUST, JUSTIFICATION and DECISION. On phones it is one column, and for program members the decision moves up under the figures.
- Times are UTC and say so. Hours are programs' one-decimal hours (`12.4 h`). Line amounts and day totals use the usual `4h 00m`.
- The decided stamp is inverted ink with a second hairline rule 3 px outside it, like a rubber stamp, so it never reads as a solid ink button. The small stamp in list rows has no rule.
- On desktop the rail stays in view (sticky) while the document scrolls, on windows at least 760 px tall. On phones the page stacks in source order; the decision (for members) or the justification (for the submitter) is rendered again under the figures and the rail's copy hides, so tab order matches what people see.
- Days strips share one window of the day: the active hours with at least half an hour either side, on whole hours. Commit ticks closer than a 24 px target share one button whose detail lists each commit.
- Warnings (a banned or switched Hackatime account) are sans sentences with a 2 px ink rule on the left. They are never coloured.

## Submissions

- The list is a run list: an ink glyph (○ verifying, ● ready, ✕ failed, — withdrawn), never a pill. Counted hours and their evidence (`870 min verified`, `2 differ · 35 unbacked`) stay on one line each on desktop; on phones they and the dateline wrap rather than cut the program's name.
- Row actions start on their column's left edge, so Copy link lines up from row to row whatever else a row offers. While Withdraw asks, Copy link steps aside.
- A command people paste is shown whole and copyable. Its placeholders are caps words (`PROJECT`), never `<angle brackets>`, which a shell reads as redirects.
- "Submit these hours" reuses the settings form layout (label left, field right), a little wider so the command fits on one line.

## Apps and consent

- **An app's page** is a record: FACTS beside CREDENTIALS on the 8/4 grid, the INBOX across the page (it needs the width), REVIEWERS and the owner's DELETE APP under it. The app's mark is the identicon of its slug; there are no uploaded logos.
- **Owner-only actions are hidden** for reviewers, never shown disabled.
- **A one-time secret** sits in a `CopyField`, masked until "Show", with one ink sentence saying it can't be shown again. Afterwards only "Set <date> UTC" is shown, never a masked value that looks retrievable.
- **Mono fields:** `.field` resets the font, so fields for URIs, slugs and usernames add `.field.mono` locally.
- **The consent page** has only the wordmark in its header. One sheet about 520 px wide, centred in space, holds the two identicons (the app's → yours), "<App> by <owner> wants to:", scope rows with their label in `.lbl` on the right, the host you go back to in mono, then Allow and Deny side by side from the left edge, at the same 44 px height. The trust sentence and the way to remove access sit under the sheet as `.hint`. No checkboxes per scope (there are no partial grants) and no colour.
- **Dead ends are sentences without buttons:** an ended request or a broken app link says what happened and what to do, and offers no button that would only fail.

## Words

- **Counted:** credited by the agent.
- **At the keyboard:** raw editor time.
- **Agent alone, idle:** time that wasn't counted.

Never call raw time "coding time" or "hours worked". Sentences are plain and short, in second person. Example content in the UI is labelled "example".

## Signed-out home

- One promise, one action, one picture. The headline, a single sentence and "Sign in with Hack Club" are all the hero holds; the reading links live in the header and footer.
- The example is one figure that teaches one idea: an hour as a single strip, with each cut labelled under the minutes it took. Labels go on the data, not in a legend or a log table beside it.
- Privacy copy names exactly what reaches the server (project, language and file names, counted minutes, the daily hash). "Only timings leave your laptop" was wrong and was removed.

## Don'ts (already tried and rejected)

- A dotted background, corner ticks and an outer frame around the page.
- Bright white index tabs on panel heads.
- An evidence panel on the dashboard (evidence belongs to submissions).
- Fake logos or avatars from other services: users get a pixel identicon drawn from their username.
- Decorative bits that answer no question.
- "Fig. 1" style numbering when there is no Fig. 2.
