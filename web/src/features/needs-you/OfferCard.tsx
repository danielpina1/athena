import { Card } from "@/ui/athena/Card";
import { Button } from "@/ui/primitives/button";
import { toast } from "@/ui/primitives/toast";
import type { PromotionOffer } from "@/gen/athena/v1/state_pb";
import { allowAction, dismissOffer, errorMessage } from "@/lib/commands/actions";

/** An action the owner keeps approving, offered as a standing allow rule. Allowing it asks for the passkey again. */
export function OfferCard({ offer }: { offer: PromotionOffer }) {
  const run = async (allow: boolean) => {
    const r = await (allow ? allowAction : dismissOffer)(offer.class);
    if (!r.ok) {
      toast({ title: allow ? "Could not allow it" : "Could not dismiss", description: errorMessage(r.code), tone: "danger" });
    }
  };
  return (
    <Card role="group" aria-label={`Always allow ${offer.tool}`} tabIndex={-1} className="grid gap-3">
      <p className="break-words">
        You approved <span className="font-mono font-semibold">{offer.tool}</span> ·{" "}
        <span className="font-mono">{offer.rule}</span> in {offer.repo} {offer.approved} times in a row. Always allow it here?
      </p>
      <p className="text-sm text-muted">
        Writes a standing rule to your Policy. Never applies to work from outside your allow list.
      </p>
      <div className="grid grid-cols-2 gap-3">
        <Button onClick={() => void run(false)}>Not now</Button>
        <Button variant="accent" onClick={() => void run(true)}>
          Always allow
        </Button>
      </div>
    </Card>
  );
}
