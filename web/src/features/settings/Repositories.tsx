// Settings › Repositories: for each GitHub repository whether Athena acts on it for real (Live) or holds its replies,
// pushes and pull requests for the owner (Shadow), and how many things may wait on the owner there before Athena stops
// starting work it chose itself. A default line sets that number for every repository without its own. Saved with the
// rest of the Policy (setPolicy), against the revision the owner edited from.
import { useCallback, useId, useMemo } from "react";
import { useAthena } from "@/lib/store/state";
import { Waiting } from "@/ui/athena/Waiting";
import { Select } from "@/ui/primitives/select";
import { usePolicyDoc, savePolicy } from "./PolicyForm";
import {
  DEFAULT_WIP,
  equalRepositories,
  readRepositories,
  repoKey,
  repoSetting,
  type Repositories as RepositoriesValue,
  WIP_RANGE,
  writeRepositories,
} from "./brakes";
import { SaveBar, useSaveBlock } from "./SaveBar";
import { Block, Section } from "./Section";

const range = (lo: number, hi: number) => Array.from({ length: hi - lo + 1 }, (_, i) => lo + i);

export function Repositories() {
  const { doc, revision, ready } = usePolicyDoc();
  const all = useAthena((s) => s.repos);
  const repos = useMemo(() => all.filter((r) => r.kind === "github"), [all]);
  const server = useMemo(() => (doc ? readRepositories(doc) : undefined), [doc]);
  const send = useCallback((r: RepositoriesValue, base: number) => savePolicy(base, (d) => writeRepositories(d, r)), []);
  const block = useSaveBlock({ label: "Repositories", server, revision, equal: equalRepositories, send });
  const id = useId();
  const value = block.value;

  const set = (name: string, patch: { mode?: string; wip?: number | undefined }) =>
    block.edit((r) => ({ ...r, repos: { ...r.repos, [repoKey(name)]: { ...repoSetting(r, name), ...patch } } }));

  return (
    <Section title="Repositories" text="Whether Athena acts on each GitHub repository for real, and how much it may start there.">
      {!ready || !value ? (
        <Waiting className="px-4 py-6 lg:px-6" />
      ) : (
        <>
          <Block title="Default" id={`${id}-default`}>
            <label className="grid max-w-sm gap-1 text-sm">
              <span className="font-semibold">Hold self-chosen work when this many things wait on you</span>
              <Select value={value.wip} onChange={(e) => block.edit((r) => ({ ...r, wip: Number(e.target.value) }))}>
                {range(...WIP_RANGE).map((n) => (
                  <option key={n} value={n}>
                    {n === DEFAULT_WIP ? `${n} (default)` : n}
                  </option>
                ))}
              </Select>
              <span className="text-muted">Applies to every repository below that has no number of its own.</span>
            </label>
          </Block>
          <Block title="Repositories">
            {repos.length === 0 ? (
              <p className="max-w-prose text-sm text-muted">No GitHub repository is known yet.</p>
            ) : (
              <ul className="grid gap-3">
                {repos.map((r) => {
                  const s = repoSetting(value, r.name);
                  return (
                    <li key={r.name} className="grid min-w-0 gap-3 rounded-md border border-line-soft p-3">
                      <div className="grid gap-0.5">
                        <h4 className="font-semibold break-words">{r.name}</h4>
                        <p className="text-sm text-muted">
                          {r.owed} of {r.ceiling || value.wip} waiting on you
                          {r.braked > 0 && `, ${r.braked} held back`}
                        </p>
                      </div>
                      <div className="grid gap-3 sm:grid-cols-2">
                        <label className="grid min-w-0 gap-1 text-sm">
                          <span className="font-semibold">{`Mode of ${r.name}`}</span>
                          <Select value={s.mode} onChange={(e) => set(r.name, { mode: e.target.value })}>
                            <option value="live">Live</option>
                            <option value="shadow">Shadow (held for you)</option>
                          </Select>
                        </label>
                        <label className="grid min-w-0 gap-1 text-sm">
                          <span className="font-semibold">{`Hold work in ${r.name} when this many wait on you`}</span>
                          <Select
                            value={s.wip ?? ""}
                            onChange={(e) => set(r.name, { wip: e.target.value === "" ? undefined : Number(e.target.value) })}
                          >
                            <option value="">{`${value.wip} (default)`}</option>
                            {range(...WIP_RANGE).map((n) => (
                              <option key={n} value={n}>
                                {n}
                              </option>
                            ))}
                          </Select>
                        </label>
                      </div>
                    </li>
                  );
                })}
              </ul>
            )}
            <p className="max-w-prose text-sm text-muted">
              Shadow keeps replies, pushes and pull requests held for you in Needs you; nothing reaches GitHub until you apply it.
            </p>
          </Block>
          <SaveBar label="Repositories" {...block.bar} />
        </>
      )}
    </Section>
  );
}
