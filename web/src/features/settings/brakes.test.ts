import { expect, test } from "vitest";
import { equalRepositories, equalTrust, readRepositories, readTrust, writeRepositories, writeTrust } from "./brakes";
import { parsePolicy } from "./policy";

const doc = {
  mode: "observe",
  sources: { github: { allowed: ["ana"] } },
  future: { kept: true },
  wipCeiling: 4,
  repos: { "acme/web": { mode: "shadow", wipCeiling: 2, note: "x" }, "acme/api": { wipCeiling: 6 } },
  trust: { stranger: "restricted", owner: "standard", extra: "keep" },
  allow: [{ tool: "Bash", rule: "make test", repo: "acme/web", added: "2026-11-01T10:00:00Z", more: 1 }],
  promoteAfter: 8,
};
const json = JSON.stringify(doc);

test("repositories round-trip without touching other keys", () => {
  const r = readRepositories(parsePolicy(json));
  expect(r.wip).toBe(4);
  expect(r.repos["acme/web"]).toEqual({ mode: "shadow", wip: 2 });
  expect(r.repos["acme/api"]).toEqual({ mode: "live", wip: 6 });
  expect(JSON.parse(writeRepositories(parsePolicy(json), r))).toEqual(doc);

  const out = JSON.parse(
    writeRepositories(parsePolicy(json), { wip: 3, repos: { ...r.repos, "acme/web": { mode: "live" }, "acme/new": { mode: "shadow" } } }),
  );
  expect(out.wipCeiling).toBeUndefined();
  expect(out.repos["acme/web"]).toEqual({ note: "x" });
  expect(out.repos["acme/new"]).toEqual({ mode: "shadow" });
  expect(out.repos["acme/api"]).toEqual({ wipCeiling: 6 });
  expect({ ...out, wipCeiling: 4, repos: doc.repos }).toEqual(doc);
});

test("a repository back to live with the default ceiling leaves no entry", () => {
  const r = readRepositories(parsePolicy(json));
  const out = JSON.parse(writeRepositories(parsePolicy(json), { ...r, repos: { "acme/api": { mode: "live" }, "acme/web": { mode: "live" } } }));
  expect(out.repos).toEqual({ "acme/web": { note: "x" } });
  expect(equalRepositories(r, readRepositories(parsePolicy(JSON.stringify(out))))).toBe(false);
});

test("trust, standing rules and promoteAfter round-trip without touching other keys", () => {
  const t = readTrust(parsePolicy(json));
  expect(t.trust.stranger).toBe("restricted");
  expect(t.trust.bot).toBe("standard");
  expect(t.promoteAfter).toBe(8);
  expect(t.allow.map((a) => a.rule)).toEqual(["make test"]);
  const out = JSON.parse(writeTrust(parsePolicy(json), t));
  expect(out).toEqual({ ...doc, trust: { ...doc.trust, allowed: "standard", collaborator: "standard", bot: "standard", ci: "standard" } });

  const edited = JSON.parse(writeTrust(parsePolicy(json), { ...t, allow: [], promoteAfter: 5, trust: { ...t.trust, bot: "restricted", owner: "restricted" } }));
  expect(edited.allow).toBeUndefined();
  expect(edited.promoteAfter).toBeUndefined();
  expect(edited.trust.bot).toBe("restricted");
  expect(edited.trust.owner).toBe("standard");
  expect(edited.trust.extra).toBe("keep");
  expect(equalTrust(t, readTrust(parsePolicy(JSON.stringify(edited))))).toBe(false);
});

test("an empty document reads as the defaults", () => {
  expect(readRepositories({})).toEqual({ wip: 3, repos: {} });
  const t = readTrust({});
  expect(t.promoteAfter).toBe(5);
  expect(t.trust.stranger).toBe("restricted");
  expect(t.allow).toEqual([]);
});
