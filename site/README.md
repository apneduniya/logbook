# Logbook docs

The documentation site for Logbook, published at https://docs.logbook.boogles.dev. It is built with [Astro Starlight](https://starlight.astro.build).

The docs are split by audience. Each audience is a **topic** with its own sidebar:

| Folder                           | Who it's for       |
| -------------------------------- | ------------------ |
| `src/content/docs/users/`        | People who code    |
| `src/content/docs/reviewers/`    | Reviewers          |
| `src/content/docs/programs/`     | Program organisers |
| `src/content/docs/fraud-squad/`  | Fraud Squad        |

## Edit a page (no setup needed)

1. Open the page on the docs site.
2. Scroll to the bottom and click **Edit page**. GitHub opens the page's file.
3. Click the pencil icon, make your change, then click **Commit changes…** and choose **Create a new branch and start a pull request**.
4. Someone reviews the pull request and merges it. The site updates after the next deploy.

You can also browse to the file on GitHub yourself: every page is a file under `site/src/content/docs/`, and its address matches its path. `docs.logbook.boogles.dev/users/install/` is `site/src/content/docs/users/install.md`.

## Writing a page

Each page starts with a short header between `---` lines:

```md
---
title: Install Logbook
description: Install the Logbook agent on Windows, macOS or Linux.
---
```

The title becomes the page heading, so don't repeat it as a `#` heading. The description is shown in search results and link previews: one plain sentence.

### Markdown basics

```md
## A section heading

A paragraph is just text. Leave a blank line between paragraphs.

**Bold** for the word that matters, _italic_ rarely.

- A bullet list
- has a dash per line

1. A numbered list
2. is for steps in order

[A link to another page](/users/what-counts/)

`a command or file name` in backticks.
```

For a command people paste, use a fenced code block with its language:

````md
```sh
logbook status
```
````

A note that stands out:

```md
:::note
Your editor needs to be open for time to be counted.
:::
```

Use `:::tip`, `:::caution` or `:::danger` for the other kinds.

Links to other docs pages start with `/` and end with `/`, like `/users/install/`. The build checks every link, so a typo fails it instead of shipping a broken link.

### Screenshots

Screenshots live in `src/assets/shots/`. Name them in lowercase with dashes, such as `web-review.png`.

To use one, the page must end in `.mdx` instead of `.md` (rename it), and import the `Shot` component under the header:

```mdx
---
title: Open a review link
description: Open the review page a submitter sent you.
---

import Shot from '../../../components/Shot.astro';

<Shot name="web-review.png" alt="The review page, with counted hours as the biggest figure" />

<Shot name="web-review.png" alt="..." caption="An optional sentence shown under the image." />
```

- `alt` is required. Describe what the screenshot shows for someone who can't see it.
- If the file isn't in `src/assets/shots/`, the build fails and names the missing file.
- Screenshots use made-up example data only: no real names, accounts or projects.
- Images are resized and compressed automatically, and readers can click to zoom.

### Tabs for Windows, macOS and Linux

When steps differ by operating system, use tabs. Always use `syncKey="os"` and the labels `Windows`, `macOS` and `Linux` in that order, so choosing an OS on one page chooses it everywhere. The page must be `.mdx`.

```mdx
import { Tabs, TabItem } from '@astrojs/starlight/components';

<Tabs syncKey="os">
	<TabItem label="Windows">Steps for Windows.</TabItem>
	<TabItem label="macOS">Steps for macOS.</TabItem>
	<TabItem label="Linux">Steps for Linux.</TabItem>
</Tabs>
```

### Adding a new page

1. Create the file in the right folder, such as `src/content/docs/users/my-page.md`.
2. Add its slug (`my-page`) to that topic's list in `topics.mjs`, where it should appear in the sidebar.

The build fails if a page isn't listed in any topic.

## Run it locally

You need [Node.js](https://nodejs.org) 22.23 or later and [pnpm](https://pnpm.io).

From the repository root:

```sh
make docs-dev     # live preview at http://localhost:4321, reloads as you edit
make docs-build   # production build into site/dist
make docs-check   # the build plus its checks: links, anchors, sidebars, screenshots
```

Or from this folder: `pnpm install`, then `pnpm dev` or `pnpm build`.

## How it's put together

- `astro.config.mjs`: site settings and plugins.
- `topics.mjs`: the four audiences and the pages in each sidebar.
- `src/content/docs/`: the pages.
- `src/components/Shot.astro`: the screenshot component.
- `src/styles/`: fonts and the theme, which follows `web/DESIGN.md`.
- `public/fonts/`: Departure Mono, Geist and Geist Mono, self-hosted under the SIL Open Font License 1.1 (licence files beside them).

Plugins: [starlight-sidebar-topics](https://starlight-sidebar-topics.netlify.app/) for one sidebar per audience, [starlight-image-zoom](https://starlight-image-zoom.vercel.app/) for zoomable screenshots, and [starlight-links-validator](https://starlight-links-validator.vercel.app/) to fail the build on broken links. Search is Starlight's built-in Pagefind.

## Publish an update

Publishing is a manual Cloudflare Pages deploy. Editing or merging a page on GitHub does not deploy it automatically.

With Cloudflare credentials already available in your environment, run from the repository root:

```sh
make docs-check
wrangler pages deploy site/dist --project-name logbook-docs --branch main
```

The project serves `https://docs.logbook.boogles.dev` and `https://logbook-docs-e0o.pages.dev`. The `main` argument selects the Pages production deployment; it does not push or merge a Git branch.

The initial documentation source is on `docs/site`. The **Edit page** URL in `astro.config.mjs` points to that branch; change it to `main` when the docs are merged there. Publishing source changes to GitHub and deploying site updates need the maintainer's approval under the repository rules.
