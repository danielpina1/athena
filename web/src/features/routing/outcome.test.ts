import { expect, test } from "vitest";
import { reasonSentence } from "./outcome";

test("the brake reads as a sentence naming the repository and the count; other codes have none", () => {
  expect(reasonSentence("wip_brake", { repo: "acme/web", owed: "3", ceiling: "3" })).toBe(
    "Held: 3 things already wait on you in acme/web (limit 3).",
  );
  expect(reasonSentence("wip_brake", { repo: "acme/web", owed: "1", ceiling: "1" })).toBe(
    "Held: 1 thing already waits on you in acme/web (limit 1).",
  );
  expect(reasonSentence("routed", { ticket: "T-2" })).toBeUndefined();
});

test("a gone Autopilot entry reads as a sentence", () => {
  expect(reasonSentence("autopilot_gone", {})).toBe("Not started: this Autopilot entry is off or was removed.");
});
