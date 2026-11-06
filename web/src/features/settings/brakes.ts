// Repositories and trust as Settings edits them (internal/policy/settings.go: repos, wipCeiling, trust, allow,
// promoteAfter), read from and written over the Policy's settings document like policy.ts does for the other blocks.
// Kept apart from policy.ts, which the initial route imports (features/control/trust.tsx): this stays in the lazy
// Settings chunk.
import { isObject, type Json, str } from "./policy";

// --- repositories and trust (internal/policy/settings.go: repos, wipCeiling, trust, allow, promoteAfter) ---

export const DEFAULT_WIP = 3;
export const WIP_RANGE = [1, 20] as const;
export const DEFAULT_PROMOTE = 5;
export const PROMOTE_RANGE = [2, 50] as const;
export const REPO_MODES = ["live", "shadow"] as const;
export const TRUST_CLASSES = ["owner", "allowed", "collaborator", "stranger", "bot", "ci"] as const;
export const TRUST_LEVELS = ["standard", "restricted"] as const;
export type TrustClass = (typeof TRUST_CLASSES)[number];
export type TrustLevel = (typeof TRUST_LEVELS)[number];

/** One repository's setting: `wip` is undefined while it follows the default for all repositories. */
export type RepoSetting = { mode: string; wip?: number };
/** Keyed by the lower-case owner/name; `wip` is the default for repositories without their own ceiling. */
export type Repositories = { wip: number; repos: Record<string, RepoSetting> };
export type AllowRule = { tool: string; rule: string; repo: string; added: string; orig: Json };
export type Trust = { trust: Record<TrustClass, TrustLevel>; allow: AllowRule[]; promoteAfter: number };

const positive = (v: unknown, fallback: number) => (typeof v === "number" && v > 0 ? v : fallback);

/** The key a repository is stored under. */
export const repoKey = (name: string) => name.toLowerCase();

export function readRepositories(doc: Json): Repositories {
  const repos: Record<string, RepoSetting> = {};
  if (isObject(doc.repos)) {
    for (const [name, r] of Object.entries(doc.repos)) {
      if (!isObject(r)) continue;
      const wip = typeof r.wipCeiling === "number" && r.wipCeiling > 0 ? r.wipCeiling : undefined;
      repos[repoKey(name)] = { mode: r.mode === "shadow" ? "shadow" : "live", wip };
    }
  }
  return { wip: positive(doc.wipCeiling, DEFAULT_WIP), repos };
}

export function writeRepositories(doc: Json, r: Repositories): string {
  const prev = isObject(doc.repos) ? doc.repos : {};
  const out: Json = {};
  // Entries this client did not touch (and their unknown fields) stay as they were.
  for (const [name, v] of Object.entries(prev)) if (!(repoKey(name) in r.repos)) out[name] = v;
  for (const [key, s] of Object.entries(r.repos)) {
    const name = Object.keys(prev).find((n) => repoKey(n) === key) ?? key;
    const entry: Json = isObject(prev[name]) ? { ...(prev[name] as Json) } : {};
    if (s.mode === "shadow") entry.mode = "shadow";
    else delete entry.mode;
    if (s.wip !== undefined) entry.wipCeiling = s.wip;
    else delete entry.wipCeiling;
    if (Object.keys(entry).length > 0) out[name] = entry;
  }
  const next: Json = { ...doc, repos: out };
  if (r.wip === DEFAULT_WIP) delete next.wipCeiling;
  else next.wipCeiling = r.wip;
  if (Object.keys(out).length === 0) delete next.repos;
  return JSON.stringify(next);
}

/** What a repository shows: the draft's setting, else live with the default ceiling. */
export const repoSetting = (r: Repositories, name: string): RepoSetting => r.repos[repoKey(name)] ?? { mode: "live" };

export function equalRepositories(a: Repositories, b: Repositories): boolean {
  if (a.wip !== b.wip) return false;
  const keys = new Set([...Object.keys(a.repos), ...Object.keys(b.repos)]);
  return [...keys].every((k) => {
    const x = a.repos[k] ?? { mode: "live" };
    const y = b.repos[k] ?? { mode: "live" };
    return x.mode === y.mode && x.wip === y.wip;
  });
}

/** internal/policy: a stranger is restricted until the owner says otherwise. */
const TRUST_DEFAULT: Record<TrustClass, TrustLevel> = {
  owner: "standard",
  allowed: "standard",
  collaborator: "standard",
  stranger: "restricted",
  bot: "standard",
  ci: "standard",
};

export function readTrust(doc: Json): Trust {
  const t = isObject(doc.trust) ? doc.trust : {};
  const trust = { ...TRUST_DEFAULT };
  for (const c of TRUST_CLASSES) if (t[c] === "standard" || t[c] === "restricted") trust[c] = t[c];
  trust.owner = "standard";
  const allow = Array.isArray(doc.allow) ? doc.allow.filter(isObject) : [];
  return {
    trust,
    allow: allow.map((a) => ({ tool: str(a.tool), rule: str(a.rule), repo: str(a.repo), added: str(a.added), orig: a })),
    promoteAfter: positive(doc.promoteAfter, DEFAULT_PROMOTE),
  };
}

export function writeTrust(doc: Json, t: Trust): string {
  const prev = isObject(doc.trust) ? doc.trust : {};
  const next: Json = { ...doc, trust: { ...prev, ...Object.fromEntries(TRUST_CLASSES.filter((c) => c !== "owner").map((c) => [c, t.trust[c]])) } };
  if (t.allow.length > 0) next.allow = t.allow.map((a) => a.orig);
  else delete next.allow;
  if (t.promoteAfter === DEFAULT_PROMOTE) delete next.promoteAfter;
  else next.promoteAfter = t.promoteAfter;
  return JSON.stringify(next);
}

export function equalTrust(a: Trust, b: Trust): boolean {
  const same = (x: AllowRule, y: AllowRule) => x.tool === y.tool && x.rule === y.rule && x.repo === y.repo && x.added === y.added;
  return (
    a.promoteAfter === b.promoteAfter &&
    TRUST_CLASSES.every((c) => a.trust[c] === b.trust[c]) &&
    a.allow.length === b.allow.length &&
    a.allow.every((r, i) => same(r, b.allow[i]))
  );
}
