import { expect, test } from "vitest";
import { reasonSentence } from "./outcome";

test("the brake reads as a sentence naming the repository and the count; other codes have none", () => {
  expect(reasonSentence("wip_brake", { repo: "acme/web", owed: "3", ceiling: "3" })).toBe(
    "Held: 3 things already wait on you in acme/web (limit 3).",
  );
  expect(reasonSentence("routed", { ticket: "T-2" })).toBeUndefined();
});
