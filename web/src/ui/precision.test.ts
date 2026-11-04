import { expect, test } from "vitest";
import { cn } from "@/ui/cn";

const sources = import.meta.glob(["./primitives/*.tsx", "./athena/*.tsx", "!./**/*.test.tsx"], {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

test("components use no shadows and no gradients", () => {
  expect(Object.keys(sources).length).toBeGreaterThan(20);
  const offenders = Object.entries(sources)
    .filter(([, src]) => /\b(shadow|drop-shadow|inset-shadow|bg-gradient|bg-linear|bg-radial|bg-conic)-/.test(src))
    .map(([file]) => file);
  expect(offenders).toEqual([]);
});

test("cn keeps a token font size next to a token colour", () => {
  expect(cn("text-body text-text", "text-muted")).toBe("text-body text-muted");
  expect(cn("text-xs", "text-body")).toBe("text-body");
  expect(cn("rounded-md", "rounded-xs")).toBe("rounded-xs");
});
