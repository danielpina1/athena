import { expect, test } from "vitest";
import {
  autopilotSlug,
  CRON_PRESETS,
  type Entry,
  equalAutopilot,
  readAutopilot,
  validateAutopilot,
  writeAutopilot,
} from "./autopilot";

const doc = {
  mode: "auto",
  future: 1,
  autopilot: [
    { name: "Night", instructions: "Tidy up", repo: "acme/web", enabled: true, cron: "0 3 * * *", note: "kept" },
    { name: "Red", instructions: "Fix CI", repo: "acme/api", enabled: false, event: "ci_red" },
    { name: "Deps", instructions: "Bump", repo: "acme/api", enabled: true, event: "manifest_changed" },
  ],
};

const entry = (patch: Partial<Entry> = {}): Entry => ({
  key: "n1",
  name: "Night",
  instructions: "Tidy up",
  repo: "acme/web",
  enabled: true,
  when: "cron",
  cron: "0 3 * * *",
  ciRedHours: 4,
  orig: {},
  ...patch,
});

test("reads each entry with its trigger, and ciRedHours defaults to 4", () => {
  const es = readAutopilot(doc);
  expect(es.map((e) => [e.name, e.when, e.cron, e.ciRedHours, e.enabled])).toEqual([
    ["Night", "cron", "0 3 * * *", 4, true],
    ["Red", "ci_red", "", 4, false],
    ["Deps", "manifest_changed", "", 4, true],
  ]);
  expect(new Set(es.map((e) => e.key)).size).toBe(3);
  expect(readAutopilot({})).toEqual([]);
  expect(readAutopilot({ autopilot: "nope" })).toEqual([]);
});

test("writing what was read changes nothing but ciRedHours of a ci_red entry, and keeps unknown fields", () => {
  const out = JSON.parse(writeAutopilot(doc, readAutopilot(doc)));
  expect(out.future).toBe(1);
  expect(out.mode).toBe("auto");
  expect(out.autopilot[0]).toEqual(doc.autopilot[0]);
  expect(out.autopilot[0].note).toBe("kept");
  expect(out.autopilot[1]).toEqual({ ...doc.autopilot[1], ciRedHours: 4 });
  expect(out.autopilot[2]).toEqual(doc.autopilot[2]);
});

test("changing the trigger drops the fields of the old one", () => {
  const [night, red] = readAutopilot(doc);
  const out = JSON.parse(
    writeAutopilot(doc, [
      { ...night, when: "ci_red", ciRedHours: 12 },
      { ...red, when: "cron", cron: "0 * * * *" },
    ]),
  ).autopilot;
  expect(out[0]).toEqual({ name: "Night", instructions: "Tidy up", repo: "acme/web", enabled: true, event: "ci_red", ciRedHours: 12, note: "kept" });
  expect(out[1]).toEqual({ name: "Red", instructions: "Fix CI", repo: "acme/api", enabled: false, cron: "0 * * * *" });
});

test("a new entry is written with trimmed text, and the key goes when the list is empty", () => {
  const out = JSON.parse(writeAutopilot({ mode: "off" }, [entry({ name: " Night ", repo: " acme/web ", cron: " 0 3 * * * " })]));
  expect(out).toEqual({ mode: "off", autopilot: [{ name: "Night", instructions: "Tidy up", repo: "acme/web", enabled: true, cron: "0 3 * * *" }] });
  const empty = JSON.parse(writeAutopilot(doc, []));
  expect(empty).toEqual({ mode: "auto", future: 1 });
  expect("autopilot" in empty).toBe(false);
});

test("equal compares what the owner edits, not the keys", () => {
  const a = readAutopilot(doc);
  expect(equalAutopilot(a, readAutopilot(doc))).toBe(true);
  expect(equalAutopilot(a, a.slice(1))).toBe(false);
  expect(equalAutopilot(a, [{ ...a[0], cron: "0 4 * * *" }, a[1], a[2]])).toBe(false);
  expect(equalAutopilot(a, [{ ...a[0], enabled: false }, a[1], a[2]])).toBe(false);
  // The hours only count for ci_red, the cron only for a schedule.
  expect(equalAutopilot(a, [{ ...a[0], ciRedHours: 9 }, a[1], a[2]])).toBe(true);
  expect(equalAutopilot(a, [a[0], { ...a[1], cron: "x" }, a[2]])).toBe(true);
  expect(equalAutopilot(a, [a[0], { ...a[1], ciRedHours: 9 }, a[2]])).toBe(false);
});

test("validation refuses what the server would", () => {
  expect(validateAutopilot([entry()])).toBeUndefined();
  expect(validateAutopilot([])).toBeUndefined();
  expect(validateAutopilot([entry({ name: "  " })])).toBe("Every entry needs a name.");
  expect(validateAutopilot([entry({ name: "-x" })])).toMatch(/must start with a letter or digit/);
  expect(validateAutopilot([entry({ name: "Night shift" }), entry({ key: "n2", name: "night_shift" })])).toBe(
    "Night shift and night_shift are the same entry; keep one.",
  );
  expect(validateAutopilot([entry({ instructions: " " })])).toBe("Night has no instructions.");
  expect(validateAutopilot([entry({ instructions: "x".repeat(4001) })])).toBe("Night's instructions are over 4000 characters.");
  expect(validateAutopilot([entry({ repo: "" })])).toBe("Night needs a repository.");
  expect(validateAutopilot([entry({ repo: "web" })])).toBe("Night needs a repository.");
  expect(validateAutopilot([entry({ cron: "0 3 * *" })])).toBe("Night needs a schedule of five fields.");
  expect(validateAutopilot([entry({ cron: "" })])).toBe("Night needs a schedule of five fields.");
  expect(validateAutopilot([entry({ when: "ci_red", cron: "", ciRedHours: 0 })])).toBe("Night: hours must be 1 to 168.");
  expect(validateAutopilot([entry({ when: "ci_red", cron: "", ciRedHours: 169 })])).toBe("Night: hours must be 1 to 168.");
  expect(validateAutopilot([entry({ when: "branch_moved", cron: "" })])).toBeUndefined();
  expect(validateAutopilot(Array.from({ length: 51 }, (_, i) => entry({ key: `n${i}`, name: `E${i}` })))).toBe("At most 50 entries.");
});

test("the slug lower-cases and joins runs of spaces and underscores with a dash", () => {
  expect(autopilotSlug("Night")).toBe("night");
  expect(autopilotSlug(" Night  shift_2 ")).toBe("night-shift-2");
});

test("the presets", () => {
  expect(CRON_PRESETS).toEqual([
    { label: "Every hour", cron: "0 * * * *" },
    { label: "Every night at 03:00", cron: "0 3 * * *" },
    { label: "Weekdays at 09:00", cron: "0 9 * * 1-5" },
    { label: "Mondays at 09:00", cron: "0 9 * * 1" },
  ]);
});
