// /graph: how Tickets, Threads and Decisions connect. A lazy chunk: the graph library Task 15 brings stays out of the
// first screen. A placeholder until then.
import { Panes } from "@/app/Panes";
import { Placeholder } from "@/app/routes";

export default function GraphView() {
  return <Panes detail={<Placeholder title="Graph" text="How Tickets, Threads and Decisions connect." />} />;
}
