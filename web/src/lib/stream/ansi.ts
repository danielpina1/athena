// Session output is stripped of terminal escapes before display: no colours in v1. The one implementation lives in
// lib/text/ansi.ts (Task 11's titles, timeline and diff use it too); the session view imports it from here.
export { stripAnsi } from "@/lib/text/ansi";
