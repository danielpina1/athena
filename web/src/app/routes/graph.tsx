// /graph: how Tickets, Threads and Decisions connect (features/graph). A lazy chunk: the graph and its layout stay out
// of the first screen.
import { Panes } from "@/app/Panes";
import { Graph } from "@/features/graph/Graph";

export default function GraphView() {
  return <Panes detail={<Graph />} />;
}
