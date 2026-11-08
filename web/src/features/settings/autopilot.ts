// The Autopilot entries of the Policy's settings JSON as Settings › Autopilot edits them (internal/policy/autopilot.go
// is the schema). Kept apart from policy.ts so the initial route does not carry it. Fields of an entry this client
// does not know are kept as they came; the key `autopilot` is left out of the document when there are no entries.
import { isObject, type Json, str } from "./policy";

export type When = "cron" | "branch_moved" | "manifest_changed" | "ci_red";

export type Entry = {
  /** A key for the screen only, never saved. */
  key: string;
  name: string;
  instructions: string;
  repo: string;
  enabled: boolean;
  when: When;
  /** Only for a schedule. */
  cron: string;
  /** Only for ci_red. */
  ciRedHours: number;
  /** The entry as the server sent it ({} for a new one): its other fields are saved back unchanged. */
  orig: Json;
};

export const WHENS: { value: When; label: string }[] = [
  { value: "cron", label: "On a schedule" },
  { value: "branch_moved", label: "When the default branch moves" },
  { value: "manifest_changed", label: "When dependency files change" },
  { value: "ci_red", label: "When CI stays red" },
];

export const CRON_PRESETS: { label: string; cron: string }[] = [
  { label: "Every hour", cron: "0 * * * *" },
  { label: "Every night at 03:00", cron: "0 3 * * *" },
  { label: "Weekdays at 09:00", cron: "0 9 * * 1-5" },
  { label: "Mondays at 09:00", cron: "0 9 * * 1" },
];

/** policy.DefaultCIRedHours, and the most the server accepts. */
export const DEFAULT_CI_RED_HOURS = 4;
export const MAX_CI_RED_HOURS = 168;
const MAX_ENTRIES = 50;
const MAX_INSTRUCTIONS = 4000;

const EVENTS: readonly string[] = ["branch_moved", "manifest_changed", "ci_red"];

export function readAutopilot(doc: Json): Entry[] {
  const list = Array.isArray(doc.autopilot) ? doc.autopilot.filter(isObject) : [];
  return list.map((e, i) => {
    const when = EVENTS.includes(str(e.event)) ? (str(e.event) as When) : "cron";
    const hours = typeof e.ciRedHours === "number" && e.ciRedHours > 0 ? e.ciRedHours : DEFAULT_CI_RED_HOURS;
    return {
      key: `s${i}`,
      name: str(e.name),
      instructions: str(e.instructions),
      repo: str(e.repo),
      enabled: e.enabled === true,
      when,
      cron: str(e.cron),
      ciRedHours: hours,
      orig: e,
    };
  });
}

export function writeAutopilot(doc: Json, entries: Entry[]): string {
  const rest = { ...doc };
  delete rest.autopilot;
  if (entries.length === 0) return JSON.stringify(rest);
  return JSON.stringify({
    ...rest,
    autopilot: entries.map((e) => {
      // The fields of the trigger the entry no longer has go; the rest of what came with it stays.
      const out: Json = { ...e.orig, name: e.name.trim(), instructions: e.instructions, repo: e.repo.trim(), enabled: e.enabled };
      delete out.cron;
      delete out.event;
      delete out.ciRedHours;
      if (e.when === "cron") out.cron = e.cron.trim();
      else out.event = e.when;
      if (e.when === "ci_red") out.ciRedHours = e.ciRedHours;
      return out;
    }),
  });
}

/** What the owner can change: the schedule counts for a schedule only, the hours for ci_red only. */
export function equalAutopilot(a: Entry[], b: Entry[]): boolean {
  return (
    a.length === b.length &&
    a.every((e, i) => {
      const o = b[i];
      return (
        e.name === o.name &&
        e.instructions === o.instructions &&
        e.repo === o.repo &&
        e.enabled === o.enabled &&
        e.when === o.when &&
        (e.when !== "cron" || e.cron === o.cron) &&
        (e.when !== "ci_red" || e.ciRedHours === o.ciRedHours)
      );
    })
  );
}

/** policy.AutopilotSlug: lower case, each run of spaces and underscores a "-". */
export function autopilotSlug(name: string): string {
  return name.trim().toLowerCase().replace(/[ _]+/g, "-");
}

const NAME = /^[A-Za-z0-9][A-Za-z0-9 _-]{0,63}$/;
const REPO = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;

/** policy.Validate's checks of the entries, except what a cron expression means (the server answers that). */
export function validateAutopilot(entries: Entry[]): string | undefined {
  if (entries.length > MAX_ENTRIES) return `At most ${MAX_ENTRIES} entries.`;
  const seen = new Map<string, string>();
  for (const e of entries) {
    const name = e.name.trim();
    if (name === "") return "Every entry needs a name.";
    if (!NAME.test(name)) return `${name} must start with a letter or digit and hold at most 64 letters, digits, spaces, _ or -.`;
    const slug = autopilotSlug(name);
    const other = seen.get(slug);
    if (other !== undefined) return `${other} and ${name} are the same entry; keep one.`;
    seen.set(slug, name);
    if (e.instructions.trim() === "") return `${name} has no instructions.`;
    if ([...e.instructions].length > MAX_INSTRUCTIONS) return `${name}'s instructions are over ${MAX_INSTRUCTIONS} characters.`;
    if (!REPO.test(e.repo.trim())) return `${name} needs a repository.`;
    if (e.when === "cron" && e.cron.trim().split(/\s+/).filter(Boolean).length !== 5) return `${name} needs a schedule of five fields.`;
    if (e.when === "ci_red" && !(Number.isInteger(e.ciRedHours) && e.ciRedHours >= 1 && e.ciRedHours <= MAX_CI_RED_HOURS))
      return `${name}: hours must be 1 to ${MAX_CI_RED_HOURS}.`;
  }
  return undefined;
}
