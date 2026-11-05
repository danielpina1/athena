// Hostile text from a session (a tool result, a title, a diff line) may carry terminal escapes and control
// characters. They are removed before display, never rendered: the layout cannot be moved by them and no escape can
// recolour or rewrite the page. Bidirectional controls, which reorder what follows them so text reads differently
// than it runs ("Trojan Source"), go too; where the owner reviews code (diffs, file names) they are shown instead,
// as markers (markBidi), so the review sees them.

// CSI (ESC [ ... final), OSC (ESC ] ... BEL or ESC \), any other two-character ESC sequence, and the 8-bit CSI.
const ESCAPES = /\u001b\[[0-?]*[ -/]*[@-~]|\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)?|\u001b[@-Z\\-_]|\u009b[0-?]*[ -/]*[@-~]/g;
// C0 and C1 controls except tab and newline (a carriage return goes too, so a line cannot be overwritten).
const CONTROLS = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/g;

// The embeddings and overrides (U+202A–U+202E), the isolates (U+2066–U+2069) and the marks (U+200E, U+200F).
const BIDI = /[\u202a-\u202e\u2066-\u2069\u200e\u200f]/g;
const HAS_BIDI = /[\u202a-\u202e\u2066-\u2069\u200e\u200f]/;

/** text without ANSI escape sequences and control characters, bidirectional ones included; tabs and newlines stay. */
export function stripAnsi(text: string): string {
  return stripEscapes(text).replace(BIDI, "");
}

/** text without ANSI escape sequences and C0/C1 controls (tabs and newlines stay); bidirectional controls stay. */
export function stripEscapes(text: string): string {
  return text.replace(ESCAPES, "").replace(CONTROLS, "");
}

/** Whether text has a bidirectional control. */
export function hasBidi(text: string): boolean {
  return HAS_BIDI.test(text);
}

/** text with each bidirectional control shown as a visible marker, ⟪U+202E⟫: it no longer reorders anything. */
export function markBidi(text: string): string {
  return text.replace(BIDI, (c) => `⟪U+${c.charCodeAt(0).toString(16).toUpperCase().padStart(4, "0")}⟫`);
}
