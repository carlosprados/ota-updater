#!/usr/bin/env node
/*
 * Parses every ```mermaid block under content/ against the SAME mermaid
 * version the theme actually ships.
 *
 * Why this exists: the diagrams were once validated with a newer mermaid than
 * Relearn bundles, which accepted syntax the shipped renderer rejected. The
 * site built green and published a diagram that rendered as "Syntax error in
 * text" for every reader. Hugo cannot catch this — mermaid runs in the
 * browser — so nothing else will.
 *
 * The version is read out of the theme's own bundle rather than pinned here,
 * so a theme upgrade cannot silently desynchronise the check from reality.
 *
 * Usage:  node tools/check-diagrams.mjs
 * Requires: hugo mod vendor  (to expose the theme's mermaid.min.js)
 */
import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

const SITE = path.resolve(import.meta.dirname, '..');
const BUNDLE = path.join(
  SITE,
  '_vendor/github.com/McShelby/hugo-theme-relearn/assets/js/mermaid/mermaid.min.js',
);

function themeMermaidVersion() {
  if (!fs.existsSync(BUNDLE)) {
    console.error(`mermaid bundle not found at ${path.relative(SITE, BUNDLE)}`);
    console.error('run `hugo mod vendor` in site/ first');
    process.exit(2);
  }
  const src = fs.readFileSync(BUNDLE, 'utf8');
  // The bundle carries several `version:"x.y.z"` strings (its own plus those
  // of vendored deps). Mermaid's is the only one on the 11.x line today; take
  // the highest major we find so this keeps working across upgrades.
  const found = [...src.matchAll(/version:"(\d+\.\d+\.\d+)"/g)].map((m) => m[1]);
  const best = found
    .map((v) => v.split('.').map(Number))
    .sort((a, b) => b[0] - a[0] || b[1] - a[1] || b[2] - a[2])[0];
  if (!best) {
    console.error('could not determine the theme mermaid version');
    process.exit(2);
  }
  return best.join('.');
}

function collect(dir) {
  return fs
    .readdirSync(dir, { withFileTypes: true })
    .flatMap((e) =>
      e.isDirectory() ? collect(path.join(dir, e.name)) : [path.join(dir, e.name)],
    )
    .filter((f) => f.endsWith('.md'));
}

const version = themeMermaidVersion();
console.log(`validating against mermaid ${version} (from the theme bundle)`);

// Install into a scratch dir so the site itself needs no node_modules.
const work = fs.mkdtempSync(path.join(process.env.TMPDIR || '/tmp', 'mmdcheck-'));
execFileSync('npm', ['install', '--silent', '--no-audit', '--no-fund',
  `mermaid@${version}`, 'jsdom'], { cwd: work, stdio: 'inherit' });

const { JSDOM } = await import(path.join(work, 'node_modules/jsdom/lib/api.js'));
const dom = new JSDOM('<!DOCTYPE html><body></body>', { pretendToBeVisual: true });
globalThis.window = dom.window;
globalThis.document = dom.window.document;
Object.defineProperty(globalThis, 'navigator', {
  value: dom.window.navigator,
  configurable: true,
});
// mermaid pulls DOMPurify for label sanitising; a no-op is enough to parse.
globalThis.DOMPurify = { sanitize: (s) => s, addHook: () => {}, setConfig: () => {} };

const mermaid = (await import(path.join(work, 'node_modules/mermaid/dist/mermaid.core.mjs'))).default;
mermaid.initialize({ startOnLoad: false, securityLevel: 'loose' });

let total = 0;
let bad = 0;
for (const file of collect(path.join(SITE, 'content'))) {
  const blocks = [...fs.readFileSync(file, 'utf8').matchAll(/```mermaid\n([\s\S]*?)```/g)];
  for (const [i, m] of blocks.entries()) {
    total++;
    try {
      await mermaid.parse(m[1]);
    } catch (err) {
      bad++;
      const rel = path.relative(SITE, file);
      console.error(`\n${rel} [diagram ${i + 1}]`);
      console.error(
        String(err?.message ?? err)
          .split('\n')
          .slice(0, 8)
          .map((l) => '  ' + l)
          .join('\n'),
      );
    }
  }
}

fs.rmSync(work, { recursive: true, force: true });

if (bad) {
  console.error(`\n${bad} of ${total} diagrams failed to parse`);
  process.exit(1);
}
console.log(`${total}/${total} diagrams parse cleanly`);
