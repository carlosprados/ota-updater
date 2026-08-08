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

## Diagrams

Diagrams are fenced ```` ```mermaid ```` blocks — Relearn's render hook turns
them into zoomable figures that follow the active light/dark theme.

They are not validated at build time, since mermaid renders in the browser.
Both `task docs-check` and the CI workflow assert that `class="mermaid"`
appears in the generated HTML, which catches the failure mode that matters: a
theme upgrade changing the render hook and silently shipping a site full of
raw code blocks.

To check the diagram *syntax* itself, render them with mermaid CLI:

```sh
npx -y @mermaid-js/mermaid-cli -i content/architecture/_index.md -o /tmp/out.md
```
