// App-wide search, pure over the store: Tickets by id or title, Triggers by their text, Threads by key, "#12" or a
// GitHub issue or pull request URL. The palette shows its hits; each hit carries the route that opens it.
import type { AthenaData } from "@/lib/store/state";

export type SearchHit =
  | { kind: "ticket"; id: string; title: string; to: string }
  | { kind: "trigger"; id: string; text: string; to: string }
  | { kind: "thread"; id: string; key: string; to: string };

/** At most this many hits per kind. */
export const MAX_HITS = 20;
/** A Trigger's text is searched from this many characters: one letter matches nearly all of them. */
const MIN_TRIGGER_QUERY = 2;

const GITHUB_URL = /^(?:https?:\/\/)?(?:www\.)?github\.com\/([^/\s]+)\/([^/\s?#]+)\/(?:pull|issues)\/(\d+)/i;
const HASH_REF = /#\d+$/;

/**
 * How Thread keys and queries are compared: lower-case, without the source prefix ("github:Owner/Name#12" and
 * "https://github.com/owner/name/pull/12" both become "owner/name#12"; "#12" stays as it is).
 */
export function normalizeThread(text: string): string {
  const t = text.trim();
  const url = GITHUB_URL.exec(t);
  if (url) return `${url[1]}/${url[2]}#${url[3]}`.toLowerCase();
  return t.replace(/^[a-z][a-z0-9_-]*:(?!\/\/)/i, "").toLowerCase();
}

function threadMatches(key: string, query: string): boolean {
  const k = normalizeThread(key);
  // A reference ends the key: "#12" must not find "#123".
  return HASH_REF.test(query) ? k.endsWith(query) : k.includes(query);
}

export const ticketRoute = (id: string) => `/tickets/${encodeURIComponent(id)}`;
export const triggerRoute = (id: string) => `/routing?${new URLSearchParams({ tab: "triggers", trigger: id })}`;
export const threadRoute = (key: string) => `/routing?${new URLSearchParams({ tab: "threads", thread: key })}`;

/** The hits for q over the store's Tickets, Triggers and Threads, in that order; none for an empty q. */
export function search(state: Pick<AthenaData, "tickets" | "triggers" | "threads">, q: string): SearchHit[] {
  const text = q.trim().toLowerCase();
  if (!text) return [];
  const hits: SearchHit[] = [];

  let n = 0;
  for (const t of state.tickets.values()) {
    if (n >= MAX_HITS) break;
    if (t.id.toLowerCase().includes(text) || t.title.toLowerCase().includes(text)) {
      hits.push({ kind: "ticket", id: t.id, title: t.title, to: ticketRoute(t.id) });
      n++;
    }
  }
  n = 0;
  if (text.length >= MIN_TRIGGER_QUERY) {
    for (const t of state.triggers.values()) {
      if (n >= MAX_HITS) break;
      if (t.text.toLowerCase().includes(text)) {
        hits.push({ kind: "trigger", id: t.id, text: t.text, to: triggerRoute(t.id) });
        n++;
      }
    }
  }
  n = 0;
  const wanted = normalizeThread(q);
  for (const th of state.threads.values()) {
    if (n >= MAX_HITS) break;
    if (threadMatches(th.key, wanted)) {
      hits.push({ kind: "thread", id: th.key, key: th.key, to: threadRoute(th.key) });
      n++;
    }
  }
  return hits;
}
