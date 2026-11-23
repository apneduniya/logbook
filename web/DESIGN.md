# Logbook web: design rules

The approved direction is **B3, mono HUD**, on the Doop canvas _Logbook web UI_. Data sits in a strict grid of hairline panels, like a ship's log read on an instrument panel. These rules say how to choose; they are not a checklist to apply everywhere.

## References (why this direction)

| Reference                                        | What we took                                                             |
| ------------------------------------------------ | ------------------------------------------------------------------------ |
| parkerhendo, "the correct aesthetic of software" | Dense data in a strict hairline grid, read like a terminal or HUD        |
| mkdir.directory                                  | Ranked lists as big rows on hairlines, counts on the right               |
| Maritime, Detent UI                              | Generous framing, near-black dark mode (never navy)                      |
| Departure Mono (Helena Zhang)                    | One pixel voice for figures and panel heads                              |
| Hackatime's dashboard (what teens know)          | Today, streak, week, projects and languages are the questions people ask |

## Tokens

The tokens live in `src/routes/layout.css` and are copied from the canvas.

- **Ground and ink:** ledger paper `#F1EFE8` and iron-gall ink `#17160F` in light; `#0E0E0D` and `#ECEAE2` in dark. The theme follows the system setting.
- **The one accent:** ultramarine, `#2740D6` in light and `#7387FF` in dark. It marks **live state only**: "tracking now", the live project's edge, and today's cell. It is never decoration, links or buttons.
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
- **Phones (under 900 px):** panels stack. Under 720 px the tabs wrap to their own row. Under 480 px project rows drop the keyboard column.

## Interaction

- The active tab or segment is an **inverted block** (ink background), never an underline.
- **Links:** muted at rest and ink on hover. Arrows (→) only on links that go somewhere else.
- **Buttons:**
  - Secondary: bordered, filling with ink on hover.
  - Primary (sign in only): solid ink.
  - Destructive actions ask once inline (`ConfirmButton`) instead of opening a modal. The trigger is a bordered button, not muted text, because it acts. Its error shows by its own row, never in a page-level notice.
- **Form fields** have an `ink-3` border at rest. A hairline-only field is invisible (1.3:1).
- **Errors on fields** show after the field is left or a save is tried, not from the first keystroke.
- **Disabled:** don't disable a submit button to signal invalid input; let it explain on submit. Buttons disable only while busy.
- **Motion:** 150 ms colour and transform transitions, plus the 8-step spinner of the live badge. Everything stops under `prefers-reduced-motion`.

## Charts

- A chart whose numbers appear only on hover also carries a visually hidden table, so screen readers get the numbers.
- Copy never promises an interaction the device lacks: hover hints are hidden on touch screens.
- On phones the year grid scrolls with fixed columns, and its window is sized to whole columns so the edge never cuts a dot in half.

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
