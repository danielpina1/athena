import { render, screen } from "@testing-library/react";
import { expect, test } from "vitest";
import { App } from "@/app/App";

test("App renders the Athena heading", () => {
  render(<App />);
  expect(screen.getByRole("heading", { name: "Athena" })).toBeTruthy();
});
