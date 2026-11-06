import { create } from "@bufbuild/protobuf";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, test, vi } from "vitest";
import { RuntimeStatusSchema, type RuntimeStatus } from "@/gen/athena/v1/state_pb";
import { RuntimePicker } from "./RuntimePicker";

const status = (name: string, ready: boolean, why = "") => create(RuntimeStatusSchema, { name, ready, why, tested: true });

function Harness({ runtimes, onChange }: { runtimes: RuntimeStatus[]; onChange?: (v: string) => void }) {
  const [value, setValue] = useState("auto");
  return (
    <RuntimePicker
      value={value}
      onChange={(v) => {
        setValue(v);
        onChange?.(v);
      }}
      runtimes={runtimes}
    />
  );
}

test("runtime picker disables an unready runtime and says why", async () => {
  const user = userEvent.setup();
  const onChange = vi.fn();
  render(<Harness runtimes={[status("claude", true), status("codex", false, "Codex is not logged in: run athena codex login")]} onChange={onChange} />);
  const group = screen.getByRole("radiogroup", { name: "Runtime" });
  expect(group).toBeTruthy();
  const auto = screen.getByRole("radio", { name: "Auto" });
  const claude = screen.getByRole("radio", { name: "Claude Code" });
  const codex = screen.getByRole("radio", { name: "Codex" }) as HTMLButtonElement;
  // The default is auto.
  expect(auto.getAttribute("aria-checked")).toBe("true");
  expect(codex.disabled).toBe(true);
  expect(codex.title).toBe("Codex is not logged in: run athena codex login");
  const described = document.getElementById(codex.getAttribute("aria-describedby") ?? "");
  // The why names the runtime already, so the line does not say it twice.
  expect(described?.textContent).toBe("Codex is not logged in: run athena codex login");
  await user.click(codex);
  expect(onChange).not.toHaveBeenCalled();
  await user.click(claude);
  expect(onChange).toHaveBeenLastCalledWith("claude");
  // Pressing the chosen one again keeps it: a runtime is always chosen.
  await user.click(claude);
  expect(claude.getAttribute("aria-checked")).toBe("true");
});

test("a runtime the node does not list is disabled; with no list at all every runtime is offered", () => {
  const { unmount } = render(<Harness runtimes={[status("claude", true)]} />);
  const codex = screen.getByRole("radio", { name: "Codex" }) as HTMLButtonElement;
  expect(codex.disabled).toBe(true);
  expect(codex.title).toBe("Not set up on this node");
  expect(document.getElementById(codex.getAttribute("aria-describedby") ?? "")?.textContent).toBe("Codex: Not set up on this node");
  unmount();
  render(<Harness runtimes={[]} />);
  expect((screen.getByRole("radio", { name: "Codex" }) as HTMLButtonElement).disabled).toBe(false);
  expect((screen.getByRole("radio", { name: "Claude Code" }) as HTMLButtonElement).disabled).toBe(false);
});
