// Nodes › Add a node: a sheet with a single-use join command (CreateNodeToken, one hour, after a passkey step-up when
// the server asks for one: the outbox's re-auth), its expiry and one sentence. The command carries the join token: it
// lives in this component's state while the sheet is open and nowhere else (never logged, never stored); closing the
// sheet drops it, and a result that lands after the close is thrown away.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { CopyIcon } from "lucide-react";
import { useRef, useState } from "react";
import { createNodeToken, errorMessage } from "@/lib/commands/actions";
import { resetTime } from "@/lib/text/runtime";
import { useNow } from "@/ui/athena/RelativeTime";
import { Button } from "@/ui/primitives/button";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/ui/primitives/sheet";
import { toast } from "@/ui/primitives/toast";

type Panel =
  | { kind: "closed" }
  | { kind: "asking" }
  | { kind: "command"; command: string; expiresAt: number }
  | { kind: "failed"; message: string };

export function AddNode() {
  const [panel, setPanel] = useState<Panel>({ kind: "closed" });
  // Each request's number: only the latest one's answer, while the sheet is open, is shown.
  const request = useRef(0);

  const ask = async () => {
    const n = ++request.current;
    setPanel({ kind: "asking" });
    const r = await createNodeToken();
    if (n !== request.current) return;
    if (r.ok && r.result.case === "nodeToken") {
      const t = r.result.value;
      setPanel({ kind: "command", command: t.command, expiresAt: t.expiresAt ? timestampMs(t.expiresAt) : Number.NaN });
    } else {
      setPanel({ kind: "failed", message: r.ok ? errorMessage("internal") : r.message || errorMessage(r.code) });
    }
  };

  const close = () => {
    request.current++;
    setPanel({ kind: "closed" });
  };

  return (
    <Sheet open={panel.kind !== "closed"} onOpenChange={(open) => !open && close()}>
      <Button variant="primary" onClick={() => void ask()} className="justify-self-start">
        Add a node
      </Button>
      <SheetContent>
        <SheetHeader>
          <SheetTitle>Add a node</SheetTitle>
          <SheetDescription>Run this on the new machine, connected to your VPN.</SheetDescription>
        </SheetHeader>
        {panel.kind === "asking" && (
          <p role="status" className="text-sm text-muted">
            Creating a join command…
          </p>
        )}
        {panel.kind === "failed" && (
          <div className="grid justify-items-start gap-3">
            <p role="alert" className="text-sm text-danger">
              {panel.message}
            </p>
            <Button onClick={() => void ask()}>Try again</Button>
          </div>
        )}
        {panel.kind === "command" && <Command command={panel.command} expiresAt={panel.expiresAt} onAgain={() => void ask()} />}
      </SheetContent>
    </Sheet>
  );
}

function Command({ command, expiresAt, onAgain }: { command: string; expiresAt: number; onAgain(): void }) {
  const now = useNow();
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command);
      toast({ title: "Command copied" });
    } catch {
      toast({ title: "Not copied", description: "Select the command and copy it.", tone: "danger" });
    }
  };
  if (expiresAt <= now) {
    return (
      <div className="grid justify-items-start gap-3">
        <p className="font-medium">Expired — create a new command</p>
        <Button variant="primary" onClick={onAgain}>
          Create a new command
        </Button>
      </div>
    );
  }
  return (
    <div className="grid min-w-0 gap-3">
      <pre className="min-w-0 overflow-x-auto rounded-md border border-line bg-stage p-3 font-mono text-xs whitespace-pre-wrap break-all text-text">
        <code data-join-command>{command}</code>
      </pre>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span data-expires className="font-mono text-xs text-muted">
          {Number.isNaN(expiresAt) ? "Works once" : `Works once, until ${resetTime(expiresAt, now)}`}
        </span>
        <Button onClick={() => void copy()}>
          <CopyIcon aria-hidden />
          Copy
        </Button>
      </div>
    </div>
  );
}
