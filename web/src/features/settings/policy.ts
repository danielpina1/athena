// The Policy's settings JSON as Settings edits it: PolicyView.settings_json read into the two blocks the owner edits
// (General: mode and rules; GitHub: the allowed list, bots and the admission table) and written back. internal/policy
// is the schema; only its fields are touched. Anything else in the document (fields this client does not know, other
// Sources, unknown fields of a rule, a row or a Source part) is kept as it came, in its place: the edit spreads the
// document and replaces the known keys.

export type Json = Record<string, unknown>;

export const MODES = ["off", "observe", "auto"] as const;

/** The admission table's rows (receiving scopes) and cells, as internal/policy names them. */
export const SCOPES = ["issues", "pulls", "comments", "ci"] as const;
export const CELLS = ["listedUnaddressed", "unlistedAddressed", "unlistedUnaddressed"] as const;
export type Scope = (typeof SCOPES)[number];
export type Cell = (typeof CELLS)[number];
export type Row = Partial<Record<Cell, string>> & Json;
export type Admission = Record<string, Row>;

/** policy.validateRow: what each cell may hold. */
const ALLOWED: Record<Cell, readonly string[]> = {
  listedUnaddressed: ["inferred", "ignore"],
  unlistedAddressed: ["explicit", "inferred", "ignore"],
  unlistedUnaddressed: ["inferred", "ignore"],
};
/** policy.defaultRow: an empty cell means this. */
const DEFAULTS: Record<Cell, string> = { listedUnaddressed: "inferred", unlistedAddressed: "ignore", unlistedUnaddressed: "ignore" };

/** The choices a cell offers; none where the row has no such cell (a CI result has no sender, so only the last). */
export function choicesFor(scope: string, cell: Cell): readonly string[] {
  if (scope === "ci" && cell !== "unlistedUnaddressed") return [];
  return ALLOWED[cell];
}

export function cellValue(row: Row | undefined, cell: Cell): string {
  const v = row?.[cell];
  return typeof v === "string" && v !== "" ? v : DEFAULTS[cell];
}

export type Rule = {
  /** A key for the screen only, never saved. */
  key: string;
  name: string;
  instructions: string;
  enabled: boolean;
  /** The rule as the server sent it ({} for a new one): its other fields are saved back unchanged. */
  orig: Json;
};
export type General = { mode: string; rules: Rule[] };
/** allowed and bots are as typed: one login per line. */
export type GitHub = { allowed: string; bots: string; admission: Admission };

export const isObject = (v: unknown): v is Json => typeof v === "object" && v !== null && !Array.isArray(v);
export const str = (v: unknown) => (typeof v === "string" ? v : "");
const strings = (v: unknown) => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);

/** The settings document; {} when it is not an object (the server always sends one). */
export function parsePolicy(json: string): Json {
  try {
    const v: unknown = JSON.parse(json);
    return isObject(v) ? v : {};
  } catch {
    return {};
  }
}

function github(doc: Json): Json {
  const sources = isObject(doc.sources) ? doc.sources : {};
  return isObject(sources.github) ? sources.github : {};
}

export function readGeneral(doc: Json): General {
  const rules = Array.isArray(doc.rules) ? doc.rules.filter(isObject) : [];
  return {
    mode: str(doc.mode),
    rules: rules.map((r, i) => ({ key: `s${i}`, name: str(r.name), instructions: str(r.instructions), enabled: r.enabled === true, orig: r })),
  };
}

export function readGitHub(doc: Json): GitHub {
  const gh = github(doc);
  const admission: Admission = {};
  if (isObject(gh.admission)) for (const [scope, row] of Object.entries(gh.admission)) if (isObject(row)) admission[scope] = row as Row;
  return { allowed: strings(gh.allowed).join("\n"), bots: strings(gh.bots).join("\n"), admission };
}

/** One login per line, trimmed, blank lines dropped. */
export function lines(text: string): string[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter((l) => l !== "");
}

export function writeGeneral(doc: Json, g: General): string {
  return JSON.stringify({
    ...doc,
    mode: g.mode,
    rules: g.rules.map((r) => ({ ...r.orig, name: r.name.trim(), instructions: r.instructions, enabled: r.enabled })),
  });
}

/** The admission rows to save: every known row with its cells (only the last for CI), other rows as they were. */
function admissionOut(orig: unknown, edited: Admission): Json {
  const out: Json = isObject(orig) ? { ...orig } : {};
  for (const scope of SCOPES) {
    const row: Json = isObject(out[scope]) ? { ...(out[scope] as Json) } : {};
    for (const cell of CELLS) {
      if (choicesFor(scope, cell).length === 0) continue;
      row[cell] = cellValue(edited[scope], cell);
    }
    out[scope] = row;
  }
  return out;
}

export function writeGitHub(doc: Json, g: GitHub): string {
  const sources = isObject(doc.sources) ? doc.sources : {};
  const gh = github(doc);
  return JSON.stringify({
    ...doc,
    sources: {
      ...sources,
      github: { ...gh, allowed: lines(g.allowed), bots: lines(g.bots), admission: admissionOut(gh.admission, g.admission) },
    },
  });
}

export function equalGeneral(a: General, b: General): boolean {
  return (
    a.mode === b.mode &&
    a.rules.length === b.rules.length &&
    a.rules.every((r, i) => {
      const o = b.rules[i];
      return r.name === o.name && r.instructions === o.instructions && r.enabled === o.enabled;
    })
  );
}

const sameList = (a: string[], b: string[]) => a.length === b.length && a.every((v, i) => v === b[i]);

export function equalGitHub(a: GitHub, b: GitHub): boolean {
  return (
    sameList(lines(a.allowed), lines(b.allowed)) &&
    sameList(lines(a.bots), lines(b.bots)) &&
    SCOPES.every((s) => CELLS.every((c) => cellValue(a.admission[s], c) === cellValue(b.admission[s], c)))
  );
}

/** policy.Validate's rule checks: a name, and no two alike. */
export function validateGeneral(g: General): string | undefined {
  const seen = new Set<string>();
  for (const r of g.rules) {
    const name = r.name.trim();
    if (name === "") return "Every rule needs a name.";
    if (seen.has(name)) return `Two rules are named ${name}.`;
    seen.add(name);
  }
  return undefined;
}
