// Smoke test of the session helpers the later specs build on: a tool call the rules escalate raises an Approval the
// dashboard counts at once, the owner's answer reaches the hook, and a Report with a question waits on the owner.
// The Ticket is stopped at the end, so the shared server is back to no open Tickets for the specs after it.
import { athena, expect, expectAccessible, signedInPage, test } from "./harness";

test("a session's approval and question reach the dashboard", async ({ page, session }) => {
  await signedInPage(page);
  const ticket = await athena("task", "Deploy the staging build");
  const needsYou = page.getByRole("navigation", { name: "Main" }).getByRole("link", { name: /Needs you/ });
  try {
    const verdict = session.requestApproval(ticket, "Bash", { command: "make deploy" });
    await expect(needsYou).toHaveText("Needs you1");
    await expectAccessible(page);

    const approvals = await athena("approvals");
    const id = approvals.split("\n").find((l) => l.includes(ticket))?.split(/\s+/)[0];
    expect(id, approvals).toBeTruthy();
    await athena("deny", id!, "--note", "not today");
    expect(await verdict).toEqual({ decision: "deny", reason: "denied by the owner: not today" });
    await expect(needsYou).toHaveText("Needs you");

    expect(await session.report(ticket, "waiting", "Staging is down", "Retry later or now?")).toBe("Report filed (waiting).");
    await expect(needsYou).toHaveText("Needs you1");
  } finally {
    await athena("stop", ticket);
  }
  await expect(needsYou).toHaveText("Needs you");
});
