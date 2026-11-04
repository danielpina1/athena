// The Ticket list's filters, kept in the URL (?stage=working&repo=acme%2Fapi&model=opus&q=text) so a filtered list can
// be shared, reloaded and returned to with the back link. Other parameters (the detail's `tab`) are left alone.
import { useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router";
import type { RepoView } from "@/gen/athena/v1/state_pb";
import type { TicketFilter } from "@/lib/store/selectors";
import { Input } from "@/ui/primitives/input";
import { Select } from "@/ui/primitives/select";
import { ToggleGroup, ToggleGroupItem } from "@/ui/primitives/toggle-group";

type Stage = NonNullable<TicketFilter["stage"]>;

const STAGES: ReadonlyArray<{ param: string; stage: Stage; label: string }> = [
  { param: "open", stage: "open", label: "Open" },
  { param: "needs-you", stage: "needsYou", label: "Needs you" },
  { param: "working", stage: "working", label: "Working" },
  { param: "idle", stage: "idle", label: "Idle" },
  { param: "ended", stage: "ended", label: "Ended" },
];

const KEYS = ["stage", "repo", "model", "q"] as const;

/** The filter the parameters say; an unknown stage is no filter. */
export function readFilters(sp: URLSearchParams): TicketFilter {
  const stage = STAGES.find((s) => s.param === sp.get("stage"))?.stage;
  return {
    ...(stage && { stage }),
    ...(sp.get("repo") && { repo: sp.get("repo") ?? undefined }),
    ...(sp.get("model") && { model: sp.get("model") ?? undefined }),
    ...(sp.get("q") && { q: sp.get("q") ?? undefined }),
  };
}

/** The query string carrying only the filters ("" when there are none): a row's link keeps them, never the tab. */
export function filterSearch(sp: URLSearchParams): string {
  const out = new URLSearchParams();
  for (const k of KEYS) {
    const v = sp.get(k);
    if (v) out.set(k, v);
  }
  const s = out.toString();
  return s ? `?${s}` : "";
}

export function hasFilters(sp: URLSearchParams): boolean {
  return KEYS.some((k) => sp.get(k));
}

export type FilterParams = {
  sp: URLSearchParams;
  /** Bumps when the URL changed from outside (back/forward, a link): local copies of it re-seed. */
  external: number;
  set(key: (typeof KEYS)[number], value: string): void;
  clear(): void;
  /** The search text as last written or read: newer than `sp` while a write is still in its transition. */
  query(): string;
};

/**
 * The filters' parameters. react-router applies a URL change in a transition, so the `sp` of one render can be older
 * than a write already made: each write is built from the latest params written (a ref), never from the render's, so
 * two changes before a commit both survive. A change that is not one of our own writes re-reads the URL.
 */
export function useFilterParams(): FilterParams {
  const [sp, setSp] = useSearchParams();
  const routerKey = sp.toString();
  const latest = useRef(new URLSearchParams(routerKey));
  /** Our writes the router has not committed yet, oldest first. */
  const pending = useRef<string[]>([]);
  const [external, setExternal] = useState(0);

  useEffect(() => {
    const i = pending.current.indexOf(routerKey);
    if (i >= 0) {
      pending.current.splice(0, i + 1);
      return;
    }
    if (pending.current.length === 0 && routerKey === latest.current.toString()) return;
    pending.current = [];
    latest.current = new URLSearchParams(routerKey);
    setExternal((n) => n + 1);
  }, [routerKey]);

  const write = (next: URLSearchParams) => {
    if (next.toString() === latest.current.toString()) return;
    latest.current = next;
    pending.current.push(next.toString());
    setSp(next, { replace: true });
  };
  return {
    sp,
    external,
    set(key, value) {
      const next = new URLSearchParams(latest.current);
      if (value) next.set(key, value);
      else next.delete(key);
      write(next);
    },
    query: () => latest.current.get("q") ?? "",
    clear() {
      const next = new URLSearchParams(latest.current);
      for (const k of KEYS) next.delete(k);
      write(next);
      setExternal((n) => n + 1); // the search box shows the cleared text
    },
  };
}

/** The search box keeps its text itself: a text field cannot be driven by a router transition (the caret would jump). */
function SearchBox({ params, inputRef }: { params: FilterParams; inputRef?: React.Ref<HTMLInputElement> }) {
  const [q, setQ] = useState(() => params.query());
  return (
    <Input
      ref={inputRef}
      type="search"
      aria-label="Search Tickets"
      placeholder="Search by title or id"
      autoComplete="off"
      value={q}
      onChange={(e) => {
        setQ(e.target.value);
        params.set("q", e.target.value);
      }}
    />
  );
}

/** Options for a select: the known values, and the one the URL names even when it is not among them. */
function withCurrent(values: readonly string[], current: string | null): string[] {
  return current && !values.includes(current) ? [...values, current] : [...values];
}

export function Filters({
  params,
  repos,
  models,
  searchRef,
}: {
  params: FilterParams;
  repos: readonly RepoView[];
  models: readonly string[];
  searchRef?: React.Ref<HTMLInputElement>;
}) {
  const { sp, set } = params;
  const stage = STAGES.find((s) => s.param === sp.get("stage"))?.param ?? "";
  return (
    <div className="grid gap-2">
      <ToggleGroup
        type="single"
        aria-label="Stage"
        value={stage}
        // Pressing the chosen segment again clears it.
        onValueChange={(v) => set("stage", v)}
      >
        {STAGES.map((s) => (
          <ToggleGroupItem key={s.param} value={s.param}>
            {s.label}
          </ToggleGroupItem>
        ))}
      </ToggleGroup>
      <div className="grid grid-cols-2 gap-2">
        <Select aria-label="Repository" value={sp.get("repo") ?? ""} onChange={(e) => set("repo", e.target.value)}>
          <option value="">All repositories</option>
          {withCurrent(
            repos.map((r) => r.name),
            sp.get("repo"),
          ).map((name) => (
            <option key={name} value={name}>
              {name}
            </option>
          ))}
        </Select>
        <Select aria-label="Model" value={sp.get("model") ?? ""} onChange={(e) => set("model", e.target.value)}>
          <option value="">All models</option>
          {withCurrent(models, sp.get("model")).map((m) => (
            <option key={m} value={m}>
              {m}
            </option>
          ))}
        </Select>
      </div>
      <SearchBox key={params.external} params={params} inputRef={searchRef} />
    </div>
  );
}
