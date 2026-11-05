// Fails when the initial route loads more than 200 KB of JS or 30 KB of CSS, gzipped: the entry chunk and its static
// imports, from the Vite manifest. Lazy chunks (dynamic imports) do not count. It also fails when a chunk that must stay
// lazy (the session view, the diff viewer, the file viewer: their sentinels) is in the initial route.
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";

const dist = join(dirname(fileURLToPath(import.meta.url)), "..", "..", "internal", "web", "ui", "dist");
const manifest = JSON.parse(readFileSync(join(dist, ".vite", "manifest.json"), "utf8"));
const budgets = { js: 200, css: 30 };

// The app's entry; the service worker (src/sw.ts, emitted as /sw.js) is an entry too but no page loads it.
const entry = Object.keys(manifest).find((k) => manifest[k].isEntry && manifest[k].src === "index.html");
if (!entry) {
  console.error("bundle-budget: no index.html entry chunk in the manifest");
  process.exit(1);
}

const seen = new Set();
const sizes = { js: 0, css: 0 };
const gz = (file) => gzipSync(readFileSync(join(dist, file))).length;
const visit = (key) => {
  if (seen.has(key)) return;
  seen.add(key);
  const chunk = manifest[key];
  sizes.js += gz(chunk.file);
  for (const css of chunk.css ?? []) sizes.css += gz(css);
  for (const imp of chunk.imports ?? []) visit(imp);
};
visit(entry);

console.log(`sw.js ${(gz("sw.js") / 1024).toFixed(1)} KB gzipped`);
let failed = false;

// Chunks that must stay lazy carry a sentinel, "athena-lazy-chunk:<name>" (their LAZY_SENTINEL constant). Inlined, the
// session view (about 9 KB) would still fit the budget, so a stray static import of it would pass unseen without this.
// Each sentinel must be in the build (in a lazy chunk), and none in the initial route.
const SENTINEL = "athena-lazy-chunk:";
const sentinels = ["athena-lazy-chunk:session-view", "athena-lazy-chunk:diff-view", "athena-lazy-chunk:doc-viewer"];
for (const key of seen) {
  const code = readFileSync(join(dist, manifest[key].file), "utf8");
  if (code.includes(SENTINEL)) {
    const name = code.slice(code.indexOf(SENTINEL)).match(/^athena-lazy-chunk:[a-z-]+/)?.[0];
    console.error(`bundle-budget: ${name} is in the initial route (${manifest[key].file}): import it lazily`);
    failed = true;
  }
}
const all = Object.values(manifest).map((c) => readFileSync(join(dist, c.file), "utf8")).join("\n");
for (const s of sentinels) {
  if (!all.includes(s)) {
    console.error(`bundle-budget: no chunk carries ${s}: its LAZY_SENTINEL is gone, so the check above cannot see it`);
    failed = true;
  }
}
for (const kind of ["js", "css"]) {
  const kb = sizes[kind] / 1024;
  console.log(`bundle ${kind} ${kb.toFixed(1)} KB / ${budgets[kind]} KB`);
  if (kb > budgets[kind]) failed = true;
}
process.exit(failed ? 1 : 0);
