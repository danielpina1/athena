import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { expect, test, vi } from "vitest";
import { NodeStatusSchema, type NodeStatus } from "@/gen/athena/v1/state_pb";
import { NodePicker } from "./NodePicker";

const node = (init: MessageInitShape<typeof NodeStatusSchema>) => create(NodeStatusSchema, { slots: 2, ...init });
const local = node({ id: "local", name: "studio", local: true, state: "online" });
const mini = node({ id: "n-mini", name: "mini", state: "online" });
const laptop = node({ id: "n-laptop", name: "laptop", state: "offline" });
const old = node({ id: "n-old", name: "old", state: "revoked" });

function Harness({ nodes, onChange }: { nodes: NodeStatus[]; onChange?: (v: string) => void }) {
  const [value, setValue] = useState("");
  return (
    <NodePicker
      value={value}
      onChange={(v) => {
        setValue(v);
        onChange?.(v);
      }}
      nodes={nodes}
    />
  );
}

test("the node picker is hidden with one node, revoked ones not counted", () => {
  const { container, unmount } = render(<Harness nodes={[local]} />);
  expect(container.textContent).toBe("");
  expect(screen.queryByRole("radiogroup", { name: "Node" })).toBeNull();
  unmount();
  render(<Harness nodes={[local, old]} />);
  expect(screen.queryByRole("radiogroup", { name: "Node" })).toBeNull();
});

test("the node picker offers Any node by default and each node; an offline one is disabled and says so", async () => {
  const user = userEvent.setup();
  const onChange = vi.fn();
  render(<Harness nodes={[local, mini, laptop, old]} onChange={onChange} />);
  const group = screen.getByRole("radiogroup", { name: "Node" });
  expect(group).toBeTruthy();
  const any = screen.getByRole("radio", { name: "Any node" });
  expect(any.getAttribute("aria-checked")).toBe("true");
  // This machine is marked; a revoked node is not offered at all.
  expect(screen.getByRole("radio", { name: /^studio/ }).textContent).toContain("this machine");
  expect(screen.queryByRole("radio", { name: /old/ })).toBeNull();
  const off = screen.getByRole("radio", { name: /^laptop/ }) as HTMLButtonElement;
  expect(off.disabled).toBe(true);
  expect(off.textContent).toContain("offline");
  await user.click(off);
  expect(onChange).not.toHaveBeenCalled();
  await user.click(screen.getByRole("radio", { name: "mini" }));
  expect(onChange).toHaveBeenLastCalledWith("n-mini");
  // Pressing the chosen one again keeps it.
  await user.click(screen.getByRole("radio", { name: "mini" }));
  expect(screen.getByRole("radio", { name: "mini" }).getAttribute("aria-checked")).toBe("true");
  await user.click(any);
  expect(onChange).toHaveBeenLastCalledWith("");
});
