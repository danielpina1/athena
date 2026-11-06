// Settings › Trust: which tools a session may use for work started by each kind of sender (Standard or Restricted),
// the standing rules the owner has allowed (each removable), and how many approvals in a row before Athena offers a
// new one. The owner is always Standard. Saved with the rest of the Policy (setPolicy).
import { useCallback, useId, useMemo } from "react";
import { Waiting } from "@/ui/athena/Waiting";
import { Button } from "@/ui/primitives/button";
import { Select } from "@/ui/primitives/select";
import { usePolicyDoc, savePolicy } from "./PolicyForm";
import {
  DEFAULT_PROMOTE,
  equalTrust,
  PROMOTE_RANGE,
  readTrust,
  type Trust as TrustValue,
  TRUST_CLASSES,
  type TrustClass,
  type TrustLevel,
  writeTrust,
} from "./brakes";
import { SaveBar, useSaveBlock } from "./SaveBar";
import { Block, Section } from "./Section";

const CLASS: Record<TrustClass, { label: string; text: string }> = {
  owner: { label: "You", text: "Work you start yourself. Always Standard." },
  allowed: { label: "Allowed people", text: "People on the GitHub allowed list." },
  collaborator: { label: "Collaborators", text: "People with access to the repository who are not on your list." },
  stranger: { label: "Strangers", text: "Anyone else who writes to a repository." },
  bot: { label: "Bots", text: "Accounts on your bots list." },
  ci: { label: "CI results", text: "Check and workflow results." },
};
const LEVEL: Record<TrustLevel, string> = { standard: "Standard tools", restricted: "Restricted tools" };

const date = (iso: string) => {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleDateString("en-GB", { day: "numeric", month: "short", year: "numeric", timeZone: "UTC" });
};

export function Trust() {
  const { doc, revision, ready } = usePolicyDoc();
  const server = useMemo(() => (doc ? readTrust(doc) : undefined), [doc]);
  const send = useCallback((t: TrustValue, base: number) => savePolicy(base, (d) => writeTrust(d, t)), []);
  const block = useSaveBlock({ label: "Trust", server, revision, equal: equalTrust, send });
  const id = useId();
  const value = block.value;

  return (
    <Section title="Trust" text="Which tools a session gets, by who started the work, and the standing rules you have allowed.">
      {!ready || !value ? (
        <Waiting className="px-4 py-6 lg:px-6" />
      ) : (
        <>
          <Block title="Who gets which tools">
            <ul className="grid gap-3">
              {TRUST_CLASSES.map((c) => (
                <li key={c} className="grid min-w-0 gap-2 rounded-md border border-line-soft p-3 sm:grid-cols-[1fr_14rem] sm:items-center">
                  <div className="grid gap-0.5">
                    <span id={`${id}-${c}`} className="font-semibold">
                      {CLASS[c].label}
                    </span>
                    <span className="text-sm text-muted">{CLASS[c].text}</span>
                  </div>
                  <Select
                    aria-labelledby={`${id}-${c}`}
                    disabled={c === "owner"}
                    value={value.trust[c]}
                    onChange={(e) => block.edit((t) => ({ ...t, trust: { ...t.trust, [c]: e.target.value as TrustLevel } }))}
                  >
                    <option value="standard">{LEVEL.standard}</option>
                    <option value="restricted">{LEVEL.restricted}</option>
                  </Select>
                </li>
              ))}
            </ul>
            <p className="max-w-prose text-sm text-muted">
              A session for work from a Restricted sender gets the narrower tool set, and no standing rule ever applies to it.
            </p>
          </Block>
          <Block title="Standing rules">
            {value.allow.length === 0 ? (
              <p className="max-w-prose text-sm text-muted">No standing rules. Athena offers one in Needs you after repeated approvals.</p>
            ) : (
              <ul className="grid gap-2">
                {value.allow.map((a, i) => (
                  <li key={`${a.tool}|${a.rule}|${a.repo}`} className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 rounded-md border border-line-soft px-3 py-2">
                    <span className="min-w-0 flex-1 text-sm break-words">
                      <span className="font-semibold">{a.tool}</span> · <span className="font-mono">{a.rule}</span> in {a.repo}, added{" "}
                      <time dateTime={a.added}>{date(a.added)}</time>
                    </span>
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={`Remove ${a.tool} · ${a.rule} in ${a.repo}`}
                      onClick={() => block.edit((t) => ({ ...t, allow: t.allow.filter((_, j) => j !== i) }))}
                    >
                      Remove
                    </Button>
                  </li>
                ))}
              </ul>
            )}
            <label className="grid max-w-sm gap-1 text-sm">
              <span className="font-semibold">Offer a standing rule after N approvals in a row</span>
              <Select value={value.promoteAfter} onChange={(e) => block.edit((t) => ({ ...t, promoteAfter: Number(e.target.value) }))}>
                {Array.from({ length: PROMOTE_RANGE[1] - PROMOTE_RANGE[0] + 1 }, (_, i) => PROMOTE_RANGE[0] + i).map((n) => (
                  <option key={n} value={n}>
                    {n === DEFAULT_PROMOTE ? `${n} (default)` : n}
                  </option>
                ))}
              </Select>
              <span className="text-muted">The same tool, rule and repository, with no denial in between.</span>
            </label>
          </Block>
          <SaveBar label="Trust" {...block.bar} />
        </>
      )}
    </Section>
  );
}
