// Hostile text from a session (a tool result, a title, a diff line) may carry terminal escapes and control
// characters. They are removed before display, never rendered: the layout cannot be moved by them and no escape can
// recolour or rewrite the page.

// CSI (ESC [ ... final), OSC (ESC ] ... BEL or ESC \), any other two-character ESC sequence, and the 8-bit CSI.
const ESCAPES = /\u001b\[[0-?]*[ -/]*[@-~]|\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)?|\u001b[@-Z\\-_]|\u009b[0-?]*[ -/]*[@-~]/g;
// C0 and C1 controls except tab and newline (a carriage return goes too, so a line cannot be overwritten).
const CONTROLS = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/g;

/** text without ANSI escape sequences and control characters; tabs and newlines stay. */
export function stripAnsi(text: string): string {
  return text.replace(ESCAPES, "").replace(CONTROLS, "");
}
