// The Ticket list's filters, kept in the URL (?stage=working&repo=acme%2Fapi&model=opus&q=text) so a filtered list can
// be shared, reloaded and returned to with the back link. Other parameters (the detail's `tab`) are left alone.
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

export function useFilterParams() {
  const [sp, setSp] = useSearchParams();
  const set = (key: (typeof KEYS)[number], value: string) =>
    setSp(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (value) next.set(key, value);
        else next.delete(key);
        return next;
      },
      { replace: true },
    );
  const clear = () =>
    setSp(
      (prev) => {
        const next = new URLSearchParams(prev);
        for (const k of KEYS) next.delete(k);
        return next;
      },
      { replace: true },
    );
  return { sp, set, clear };
}

/** Options for a select: the known values, and the one the URL names even when it is not among them. */
function withCurrent(values: readonly string[], current: string | null): string[] {
  return current && !values.includes(current) ? [...values, current] : [...values];
}

export function Filters({
  repos,
  models,
  searchRef,
}: {
  repos: readonly RepoView[];
  models: readonly string[];
  searchRef?: React.Ref<HTMLInputElement>;
}) {
  const { sp, set } = useFilterParams();
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
      <Input
        ref={searchRef}
        type="search"
        aria-label="Search Tickets"
        placeholder="Search by title or id"
        autoComplete="off"
        value={sp.get("q") ?? ""}
        onChange={(e) => set("q", e.target.value)}
      />
    </div>
  );
}
