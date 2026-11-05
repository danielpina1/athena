import { render } from "@testing-library/react";
import { expect, test } from "vitest";
import { Dot } from "./Dot";

test("went well is green, interrupted is amber, danger red, attention orange", () => {
  const cls = (tone: Parameters<typeof Dot>[0]["tone"]) => render(<Dot tone={tone} />).container.querySelector("[data-dot]")!.className;
  expect(cls("ok")).toContain("bg-success");
  expect(cls("warn")).toContain("bg-warning");
  expect(cls("danger")).toContain("bg-danger");
  expect(cls("attention")).toContain("bg-accent");
});
