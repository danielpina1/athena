// Runtimes as people say them (the Global Constraints copy), the times a quota resets at, and the Policy's review
// default: shared by New task, the Ticket list and detail, the timeline, the node card and the sidebar.

const LABEL: Record<string, string> = { claude: "Claude Code", codex: "Codex", auto: "Auto" };

/** Claude Code, Codex or Auto; any other name is shown as it is. */
export function runtimeLabel(name: string): string {
  return LABEL[name] ?? name;
}

/** The runtimes a Ticket can be pinned to, in the order every list shows them. */
export const RUNTIMES = ["claude", "codex"] as const;

const clock = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
/** "14:02": a time of day, local, 24-hour (when a node was last seen). */
export const clockTime = (at: number) => clock.format(at);
const dated = new Intl.DateTimeFormat("en-GB", { weekday: "short", day: "numeric", month: "short" });
const startOfDay = (ms: number) => new Date(ms).setHours(0, 0, 0, 0);

/** When a limit resets, seen from `from`: HH:MM on the same local day, else with its date ("Fri 13 Nov 08:00"). */
export function resetTime(at: number, from: number): string {
  const time = clock.format(at);
  return startOfDay(at) === startOfDay(from) ? time : `${dated.format(at).replace(",", "")} ${time}`;
}

const weekday = new Intl.DateTimeFormat("en-GB", { weekday: "short" });

/** resetTime for a narrow line: HH:MM on the same local day, else the weekday ("Mon"). */
export function shortResetTime(at: number, from: number): string {
  return startOfDay(at) === startOfDay(from) ? clock.format(at) : weekday.format(at);
}

/** Whether the Policy pairs a new Ticket with a review by default (its pairing.review is "on"); false when unknown. */
export function reviewByDefault(settingsJson: string | undefined): boolean {
  try {
    const p: unknown = JSON.parse(settingsJson ?? "");
    const pairing = typeof p === "object" && p !== null ? (p as { pairing?: { review?: unknown } }).pairing : undefined;
    return pairing?.review === "on";
  } catch {
    return false;
  }
}
