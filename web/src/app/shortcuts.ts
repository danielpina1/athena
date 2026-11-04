// Keyboard shortcuts: one keydown listener on the window and a registry of what is mounted. A key spec is one or more
// combos separated by spaces ("g n" is a chord: g, then n within CHORD_MS); a combo is optional modifiers and a key
// ("mod+k" is ⌘K on the Mac and Ctrl K elsewhere, "mod+Enter", "Escape", "?"). Letters and symbols match the
// character typed, so "?" needs no "shift+".
//
// Single-key shortcuts never fire while the owner types: an event from an input, a textarea, a select or editable
// content is ignored, except Esc and ⌘⏎. While a modal dialog is open only Esc reaches the registry (the dialog
// handles the rest itself), so "g n" cannot navigate the page behind the shortcut list.
import { useEffect, useRef } from "react";
import { topModal } from "@/ui/primitives/dialog";

/** How long the second key of a chord may wait for the first. */
export const CHORD_MS = 1000;

export type ShortcutOptions = { when?: () => boolean };

type Entry = { seq: string[]; run: (e: KeyboardEvent) => void; active: () => boolean };

/** The shortcut list ("?"): the keys of the Global Constraints, in their order. */
export const SHORTCUTS: ReadonlyArray<{ keys: string; action: string }> = [
  { keys: "⌘K / Ctrl K", action: "Command palette" },
  { keys: "j / k", action: "Move" },
  { keys: "⏎", action: "Open" },
  { keys: "a", action: "Approve" },
  { keys: "d", action: "Deny" },
  { keys: "r", action: "Reply" },
  { keys: "⌘⏎", action: "Interrupt the session" },
  { keys: "/", action: "Search" },
  { keys: "g n", action: "Go to Needs you" },
  { keys: "g t", action: "Go to Tickets" },
  { keys: "g g", action: "Go to Graph" },
  { keys: "?", action: "Shortcut list" },
  { keys: "Esc", action: "Close" },
];

const MODIFIERS = new Set(["Shift", "Control", "Alt", "Meta", "OS", "CapsLock", "Fn", "Dead", "Unidentified"]);
const ALIASES: Record<string, string> = { esc: "escape", return: "enter", "⏎": "enter", cmd: "mod", ctrl: "mod", meta: "mod" };
/** Combos that still work while the owner types. */
const WHILE_TYPING = new Set(["escape", "mod+enter"]);

/** One combo in canonical form: sorted modifiers, then the key; named keys lower-case, characters as typed. */
function canonical(mods: string[], key: string): string {
  const k = key.length === 1 ? key : key.toLowerCase();
  return [...new Set(mods)].sort().concat(k).join("+");
}

/** A spec's combos, canonical. */
export function parseKeys(keys: string): string[] {
  return keys
    .trim()
    .split(/\s+/)
    .map((combo) => {
      const parts = combo.split("+");
      const key = parts.pop() ?? "";
      const mods = parts.map((m) => ALIASES[m.toLowerCase()] ?? m.toLowerCase());
      return canonical(mods, ALIASES[key.toLowerCase()] ?? key);
    });
}

/** The event's combo, or undefined for a lone modifier key. */
function comboOf(e: KeyboardEvent): string | undefined {
  if (!e.key || MODIFIERS.has(e.key)) return undefined;
  const mods: string[] = [];
  if (e.metaKey || e.ctrlKey) mods.push("mod");
  if (e.altKey) mods.push("alt");
  // A typed character already says whether Shift was down ("?", "G"); a named key needs it spelled out.
  if (e.shiftKey && e.key.length > 1) mods.push("shift");
  return canonical(mods, e.key);
}

/** Whether the event comes from somewhere the owner types. */
function fromTyping(target: EventTarget | null): boolean {
  if (!(target instanceof Element)) return false;
  if (target.closest("textarea, select")) return true;
  const input = target.closest("input");
  if (input && !["checkbox", "radio", "button", "submit", "reset", "range", "color"].includes(input.type)) return true;
  if ((target as HTMLElement).isContentEditable) return true;
  // jsdom has no isContentEditable; the attribute says the same.
  return target.closest('[contenteditable]:not([contenteditable="false"])') !== null;
}

const entries: Entry[] = [];
let pending: string[] = [];
let chordTimer: ReturnType<typeof setTimeout> | undefined;

const startsWith = (seq: string[], prefix: string[]) => prefix.every((c, i) => seq[i] === c);

/** The most recently registered active entry whose sequence is buf, and whether a longer one starts with buf. */
function match(buf: string[]): { exact?: Entry; prefix: boolean } {
  let exact: Entry | undefined;
  let prefix = false;
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i];
    if (!startsWith(e.seq, buf) || !e.active()) continue;
    if (e.seq.length === buf.length) exact ??= e;
    else prefix = true;
  }
  return { exact, prefix };
}

function resetChord() {
  pending = [];
  clearTimeout(chordTimer);
  chordTimer = undefined;
}

function onKeyDown(e: KeyboardEvent) {
  if (e.isComposing) return;
  const combo = comboOf(e);
  if (!combo) return;
  if (topModal() && combo !== "escape") return resetChord();
  if (fromTyping(e.target) && !WHILE_TYPING.has(combo)) return resetChord();

  let buf = [...pending, combo];
  let m = match(buf);
  if (!m.exact && !m.prefix && pending.length > 0) {
    // Not a continuation: the key may start (or be) a shortcut of its own.
    buf = [combo];
    m = match(buf);
  }
  resetChord();
  if (m.exact) {
    e.preventDefault();
    m.exact.run(e);
    return;
  }
  if (m.prefix) {
    pending = buf;
    chordTimer = setTimeout(resetChord, CHORD_MS);
  }
}

function register(entry: Entry): () => void {
  if (entries.length === 0) window.addEventListener("keydown", onKeyDown);
  entries.push(entry);
  return () => {
    const i = entries.indexOf(entry);
    if (i >= 0) entries.splice(i, 1);
    if (entries.length === 0) {
      window.removeEventListener("keydown", onKeyDown);
      resetChord();
    }
  };
}

/**
 * Runs handler when keys are pressed (see the file comment for the spec), while the component is mounted and
 * opts.when() is true. The handler and `when` may change between renders without re-registering. When two mounted
 * shortcuts have the same keys, the one registered last wins.
 */
export function useShortcut(keys: string, handler: (e: KeyboardEvent) => void, opts?: ShortcutOptions): void {
  const latest = useRef({ handler, when: opts?.when });
  useEffect(() => {
    latest.current = { handler, when: opts?.when };
  });
  useEffect(
    () =>
      register({
        seq: parseKeys(keys),
        run: (e) => latest.current.handler(e),
        active: () => latest.current.when?.() ?? true,
      }),
    [keys],
  );
}
