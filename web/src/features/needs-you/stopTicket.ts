import { errorMessage, stop } from "@/lib/commands/actions";
import { toast } from "@/ui/primitives/toast";

/** Stops a Ticket from a card; a failure is said in a toast (the store rolls the "Stopping…" mark back). */
export async function stopTicket(id: string) {
  const r = await stop(id, "Stopped from Needs you");
  if (!r.ok) toast({ title: "Could not stop", description: errorMessage(r.code), tone: "danger" });
}
