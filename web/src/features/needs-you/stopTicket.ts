import { errorMessage, stop } from "@/lib/commands/actions";
import { toast } from "@/ui/primitives/toast";

/** Stops a Ticket from a card or its page; a failure is said in a toast (the store rolls the "Stopping…" mark back). */
export async function stopTicket(id: string, reason = "Stopped from Needs you") {
  const r = await stop(id, reason);
  if (!r.ok) toast({ title: "Could not stop", description: errorMessage(r.code), tone: "danger" });
}
