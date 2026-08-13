# Documentation site

Hugo + [Relearn](https://mcshelby.github.io/hugo-theme-relearn/), published to
GitHub Pages at <https://carlosprados.github.io/ota-updater/>.

## Working on it

```sh
task docs-serve     # live reload on http://localhost:1313/
task docs-build     # one-shot build into site/public
task docs-check     # build + assert the diagrams survived into the HTML
```

Requires **Hugo extended** (the theme uses SCSS). The Go toolchain is also
required — the theme is a Hugo Module, which is a Go module.

## Why a separate Go module

`site/go.mod` exists solely to pin the theme. Nested modules are excluded from
the parent module's package patterns, so `go build ./...` and `go test ./...`
at the repository root are unaffected.

The theme resolves to a **pseudo-version** rather than a semver tag: upstream
tags releases as `7.x.x` without the leading `v`, which the Go module proxy
does not recognise as semver. The practical effect is a build pinned to an
exact commit — reproducible, which is what a release artifact wants.

To move to a newer theme commit:

```sh
cd site && hugo mod get -u github.com/McShelby/hugo-theme-relearn
task docs-check     # verify the mermaid render hook still works
```

## Publishing

`.github/workflows/docs.yml` builds and deploys on **push of a `v*` tag**, so
the published site always corresponds to a real release rather than to
whatever is on `main`. `workflow_dispatch` is available for docs-only fixes
and for re-deploying after a Pages settings change.

The workflow stamps the release label onto the site from the tag, and takes
its base URL from the Pages API rather than from `hugo.toml`, so a fork or a
repository rename produces correct links without editing config.

## Look and feel

The site is styled to match the blog at <https://carlos.enredando.me> — same
near-black ground, 40px grid, cyan `#38bdf8` accent, and the Space Grotesk /
Inter / JetBrains Mono trio. Three files carry it:

| File | Holds |
|---|---|
| `assets/css/theme-enredando.css` | The Relearn colour variant: **custom properties only** |
| `assets/css/enredando-skin.css` | Everything structural — the grid, sharp corners, mono chrome, section rules |
| `layouts/partials/custom-header.html` | The font request and the skin `<link>` |

The split is forced, not stylistic: Relearn inlines a variant file inside a
`:root[data-r-theme-variant='enredando'] { … }` wrapper, so a plain selector
placed there ends up nested in that block. Variables go in the variant, rules
go in the skin.

`layouts/partials/logo.html` overrides the theme's wordmark to get the blog's
two-tone brand. It must keep the `logo-title` class on a direct child — the
theme hides any logo carrying neither `.logo-title` nor `.logo-image`.

There is **one variant, dark**. The blog only ever styles its dark side, so a
light option here would advertise a look the family does not have. Print still
falls back to the theme's light defaults, which is correct.

Two variable names do not mean what they look like:

- The content column comes from **`--MAIN-WIDTH-MAX`**, not `--MAIN-MAX-width`.
  Relearn reads `var(--MENU-MAX-width, var(--MAIN-WIDTH-MAX, 81.25rem))`; the
  obvious-looking name is read by nothing and fails silently.
- Relearn derives the column as `MAIN-WIDTH-MAX − MENU-L-width − 2 × 3.25rem`,
  so the 95rem here yields roughly 1060px of usable width.

## Diagrams

Diagrams are fenced ```` ```mermaid ```` blocks. They are **fixed images**:
`mermaidZoom = false`, so there is no wheel-zoom, no drag-to-pan and no reset
button. Anything wider than the column scrolls inside its own box.

Colours come from `params.mermaidInitialize` in `hugo.toml`, and the per-node
`classDef` fills in the content use the blog's palette (cyan `#38bdf8`, green
`#4ade80`, amber `#fbbf24`, red `#f87171`, grey `#6b7687`) with the `2e` alpha
suffix, so a tint composes over the page instead of cutting a hole in it.

### Rules that come from having been bitten

- **`mermaidInitialize` must be valid JSON.** Relearn hands it to `JSON.parse`
  in the browser; a broken value throws, `themeUseMermaid` is never assigned,
  and **every diagram on the site silently disappears** with a green build.
  JSON forbids literal newlines inside strings, so a multi-line comment value
  inside the TOML `'''…'''` block is enough to do it. `task docs-diagrams` now
  parses it as JSON for exactly this reason.
- **Keep diagrams under ~1050px wide.** Above the column width the browser
  scales the whole SVG down, and a flowchart at 0.5 scale has 7px labels. Long
  edge labels are the usual cause; `flowchart TB` spends a long label on height
  where `LR` spends it on width. Measure with the rendered `viewBox`, not by eye.
- **Ask Google Fonts for italics.** Without the `ital` axis the browser shears
  the roman face, and synthesised oblique ink reaches past the box mermaid drew
  to the measured width — the last letter of every `<i>` label lands outside it.
- **`themeVariables.fontSize` must match what mermaid paints (16px).** The
  sequence renderer sizes its note and message boxes from that value but writes
  `font-size: 16px` into the `<text>`; at 14px every box was 14% too narrow.
  The per-diagram `sequence.*FontSize` keys do **not** move either number.
- **`sequence.width` is a fixed participant width**, not a measured one.
  Mermaid will not grow it for a long participant name — raise the knob or
  shorten the name.
- **Note boxes are sized by participant spacing, not by their text.** A note
  that does not fit needs an explicit `<br/>`; there is no wrapping.
- **A subgraph that is an edge endpoint loses its `direction`,** and a subgraph
  with no edges at all is laid out as a separate disconnected component. Both
  produce the vertical-stack-in-a-tall-box result. If a relation cannot be
  drawn without spaghetti — "two nodes each depend on all five" is the case
  here — state it in prose and let the diagram show the layering.

### Validating

```sh
task docs-diagrams   # parse every block against the theme's exact mermaid
task docs-check      # the above, plus build + assert the diagrams reached the HTML
```

`docs-diagrams` reads the mermaid version out of the theme's own bundle rather
than from a pin, so a theme upgrade cannot desynchronise the check from what
readers actually run. The `class="mermaid"` assertion in `docs-check` catches
the other failure mode: a theme upgrade changing the render hook and shipping a
site full of raw code blocks.

Neither check measures layout. Overflowing labels and shrunken diagrams need a
real browser — load the page and compare each `svg` `viewBox` width against its
rendered width, and each label's box against its shape.
