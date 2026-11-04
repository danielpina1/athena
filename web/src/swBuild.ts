// Build-time only (vite.config.ts's precache plugin; never part of the app): what the service worker precaches and
// the version that names its cache. The version covers index.html's content as well as the hashed file names, so a
// build that changes only index.html still changes sw.js, and the browser installs the new shell.
import { createHash } from "node:crypto";

/** The part of a Rollup/Rolldown output bundle used here: file name → its asset source (index.html's is the page). */
export type BundleLike = Record<string, { source?: string | Uint8Array }>;

export function precacheBuild(bundle: BundleLike): { version: string; assets: string[] } {
  const page = bundle["index.html"]?.source;
  if (page === undefined) throw new Error("precache: the build has no index.html");
  const assets = Object.keys(bundle)
    .filter((f) => f.startsWith("assets/"))
    .sort()
    .map((f) => "/" + f);
  const hash = createHash("sha256").update(assets.join("\n")).update("\n").update(page);
  return { version: hash.digest("hex").slice(0, 12), assets };
}
