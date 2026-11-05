import { useState } from "react";
import { Input } from "@/ui/primitives/input";
import type { FilterParams } from "../tickets/Filters";
import type { RoutingKey } from "./Routing";

/** Lists render at most this many rows, then "Show more". */
export const PAGE = 200;

/** How many rows a list shows: PAGE, more on request, and back to PAGE when `reset` changes (the filters did). */
export function useShown(reset: readonly string[]) {
  const key = reset.join("\u0000");
  const [state, setState] = useState({ key, n: PAGE });
  const n = state.key === key ? state.n : PAGE;
  return { shown: n, more: () => setState({ key, n: n + PAGE }) };
}

/** A tab's search box. It keeps its text itself, as the Tickets one does: a router transition would jump the caret. */
export function RoutingSearch({
  params,
  param,
  label,
  placeholder,
}: {
  params: FilterParams<RoutingKey>;
  param: RoutingKey;
  label: string;
  placeholder: string;
}) {
  return <Box key={params.external} params={params} param={param} label={label} placeholder={placeholder} />;
}

function Box({ params, param, label, placeholder }: { params: FilterParams<RoutingKey>; param: RoutingKey; label: string; placeholder: string }) {
  const [text, setText] = useState(() => params.get(param));
  return (
    <Input
      type="search"
      data-list-search
      aria-label={label}
      placeholder={placeholder}
      autoComplete="off"
      value={text}
      onChange={(e) => {
        setText(e.target.value);
        params.set(param, e.target.value);
      }}
    />
  );
}
