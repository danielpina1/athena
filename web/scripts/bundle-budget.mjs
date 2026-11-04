// Fails when the initial route loads more than 200 KB of JS or 30 KB of CSS, gzipped: the entry chunk and its static
// imports, from the Vite manifest. Lazy chunks (dynamic imports) do not count.
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
for (const kind of ["js", "css"]) {
  const kb = sizes[kind] / 1024;
  console.log(`bundle ${kind} ${kb.toFixed(1)} KB / ${budgets[kind]} KB`);
  if (kb > budgets[kind]) failed = true;
}
process.exit(failed ? 1 : 0);
