// Settings › General (the Policy mode and the rules) and Settings › Sources › GitHub (who may start work, which bots
// count, and the admission table). Both edit PolicyView.settings_json and save it whole with setPolicy(revision,
// json): each block writes its own fields over the latest document, so the other block's fields and anything this
// client does not know are kept. Each block has its own save bar.
import { ArrowDownIcon, ArrowUpIcon, PlusIcon, Trash2Icon } from "lucide-react";
import { useCallback, useId, useMemo, useRef } from "react";
import { useLayout } from "@/app/Panes";
import { setPolicy } from "@/lib/commands/actions";
import { selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { Waiting } from "@/ui/athena/Waiting";
import { Button } from "@/ui/primitives/button";
import { Input } from "@/ui/primitives/input";
import { Switch } from "@/ui/primitives/switch";
import { Textarea } from "@/ui/primitives/textarea";
import { AdmissionTable } from "./AdmissionTable";
import {
  equalGeneral,
  equalGitHub,
  type General,
  type GitHub,
  MODES,
  noteOwnRevision,
  onlyOwnSince,
  parsePolicy,
  readGeneral,
  readGitHub,
  type Rule,
  validateGeneral,
  writeGeneral,
  writeGitHub,
} from "./policy";
import { SaveBar, useSaveBlock } from "./SaveBar";
import { Block, Section } from "./Section";

const MODE: Record<(typeof MODES)[number], { label: string; text: string }> = {
  off: { label: "Off", text: "Only explicit requests start work; nothing inferred becomes a candidate." },
  observe: {
    label: "Observe",
    text: "Inferred candidates are assessed and a positive one is recorded as “would accept”; nothing is accepted.",
  },
  auto: { label: "Auto", text: "A positive assessment of an inferred candidate accepts it." },
};

/** The Policy as the store has it, read once per change. */
function usePolicyDoc() {
  const policy = useAthena((s) => s.policy);
  const synced = useAthena(selectSynced);
  const doc = useMemo(() => (policy ? parsePolicy(policy.settingsJson) : undefined), [policy]);
  return { doc, revision: policy?.revision, ready: synced && !!policy };
}

/** Sends the block's fields written over the latest document, and remembers the revision it made. */
async function savePolicy(base: number, write: (doc: ReturnType<typeof parsePolicy>) => string) {
  const latest = useAthena.getState().policy;
  const r = await setPolicy(base, write(parsePolicy(latest?.settingsJson ?? "{}")));
  if (r.ok && r.result?.case === "policyRevision") noteOwnRevision(r.result.value);
  return r;
}

export function GeneralSettings() {
  const { doc, revision, ready } = usePolicyDoc();
  const server = useMemo(() => (doc ? readGeneral(doc) : undefined), [doc]);
  const send = useCallback((g: General, base: number) => savePolicy(base, (d) => writeGeneral(d, g)), []);
  const block = useSaveBlock({
    label: "General",
    server,
    revision,
    equal: equalGeneral,
    validate: validateGeneral,
    rebase: onlyOwnSince,
    send,
  });

  return (
    <Section title="General" text="How Athena decides which incoming work to take on.">
      {!ready || !block.value ? (
        <Waiting className="px-4 py-6 lg:px-6" />
      ) : (
        <>
          <ModePicker mode={block.value.mode} onChange={(mode) => block.edit((g) => ({ ...g, mode }))} />
          <Rules rules={block.value.rules} onChange={(rules) => block.edit((g) => ({ ...g, rules }))} />
          <SaveBar label="General" {...block.bar} />
        </>
      )}
    </Section>
  );
}

function ModePicker({ mode, onChange }: { mode: string; onChange(mode: string): void }) {
  const id = useId();
  return (
    <Block title="Policy mode" id={`${id}-title`}>
      <fieldset aria-labelledby={`${id}-title`} className="grid gap-2">
        {MODES.map((m) => (
          <label
            key={m}
            className="flex min-h-11 min-w-0 cursor-pointer items-start gap-3 rounded-md border border-line-soft px-3 py-2 has-checked:border-text"
          >
            <input
              type="radio"
              name={`${id}-mode`}
              value={m}
              checked={mode === m}
              onChange={() => onChange(m)}
              aria-describedby={`${id}-${m}`}
              className="mt-1 size-4 shrink-0 accent-(--text)"
            />
            <span className="grid min-w-0 gap-0.5">
              <span className="font-semibold">{MODE[m].label}</span>
              <span id={`${id}-${m}`} className="text-sm text-muted">
                {MODE[m].text}
              </span>
            </span>
          </label>
        ))}
      </fieldset>
    </Block>
  );
}

function Rules({ rules, onChange }: { rules: Rule[]; onChange(rules: Rule[]): void }) {
  const next = useRef(0);
  const set = (i: number, patch: Partial<Rule>) => onChange(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const move = (i: number, by: -1 | 1) => {
    const out = [...rules];
    [out[i], out[i + by]] = [out[i + by], out[i]];
    onChange(out);
  };
  return (
    <Block title="Rules">
      <p className="max-w-prose text-sm text-muted">
        Each rule describes a kind of proactive work; a Ticket it matches gets its instructions.
      </p>
      {rules.length > 0 && (
        <ol className="grid gap-3">
          {rules.map((r, i) => (
            <RuleEditor
              key={r.key}
              rule={r}
              index={i}
              last={i === rules.length - 1}
              onChange={(patch) => set(i, patch)}
              onMove={(by) => move(i, by)}
              onRemove={() => onChange(rules.filter((_, j) => j !== i))}
            />
          ))}
        </ol>
      )}
      <div>
        <Button
          onClick={() =>
            onChange([...rules, { key: `n${++next.current}`, name: "", instructions: "", enabled: true, orig: {} }])
          }
        >
          <PlusIcon aria-hidden />
          Add rule
        </Button>
      </div>
    </Block>
  );
}

function RuleEditor({
  rule,
  index,
  last,
  onChange,
  onMove,
  onRemove,
}: {
  rule: Rule;
  index: number;
  last: boolean;
  onChange(patch: Partial<Rule>): void;
  onMove(by: -1 | 1): void;
  onRemove(): void;
}) {
  const id = useId();
  const called = rule.name.trim() || `rule ${index + 1}`;
  return (
    <li className="grid min-w-0 gap-3 rounded-md border border-line-soft p-3">
      <div className="grid gap-1">
        <label htmlFor={`${id}-name`} className="text-sm font-semibold">
          Name
        </label>
        <Input
          id={`${id}-name`}
          value={rule.name}
          aria-invalid={rule.name.trim() === "" || undefined}
          onChange={(e) => onChange({ name: e.target.value })}
        />
      </div>
      <div className="grid gap-1">
        <label htmlFor={`${id}-instructions`} className="text-sm font-semibold">
          Instructions
        </label>
        <Textarea
          id={`${id}-instructions`}
          value={rule.instructions}
          onChange={(e) => onChange({ instructions: e.target.value })}
        />
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <label className="flex min-h-11 items-center gap-2 text-sm lg:min-h-0">
          <Switch checked={rule.enabled} onCheckedChange={(enabled) => onChange({ enabled })} aria-label={`${called} enabled`} />
          <span aria-hidden>Enabled</span>
        </label>
        <div className="ml-auto flex gap-1">
          <Button size="icon" variant="ghost" disabled={index === 0} onClick={() => onMove(-1)} aria-label={`Move ${called} up`}>
            <ArrowUpIcon aria-hidden />
          </Button>
          <Button size="icon" variant="ghost" disabled={last} onClick={() => onMove(1)} aria-label={`Move ${called} down`}>
            <ArrowDownIcon aria-hidden />
          </Button>
          <Button size="icon" variant="ghost" onClick={onRemove} aria-label={`Remove ${called}`}>
            <Trash2Icon aria-hidden />
          </Button>
        </div>
      </div>
    </li>
  );
}

export function GitHubSettings() {
  const layout = useLayout();
  const { doc, revision, ready } = usePolicyDoc();
  const server = useMemo(() => (doc ? readGitHub(doc) : undefined), [doc]);
  const send = useCallback((g: GitHub, base: number) => savePolicy(base, (d) => writeGitHub(d, g)), []);
  const block = useSaveBlock({ label: "GitHub", server, revision, equal: equalGitHub, rebase: onlyOwnSince, send });
  const id = useId();

  return (
    <Section title="Sources › GitHub" text="Who may start work on GitHub, and what each kind of message becomes.">
      {!ready || !block.value ? (
        <Waiting className="px-4 py-6 lg:px-6" />
      ) : (
        <>
          <Block title="People and bots">
            <div className="grid gap-4 lg:grid-cols-2">
              <div className="grid content-start gap-1">
                <label htmlFor={`${id}-allowed`} className="text-sm font-semibold">
                  Allowed
                </label>
                <Textarea
                  id={`${id}-allowed`}
                  aria-describedby={`${id}-allowed-hint`}
                  className="font-mono"
                  value={block.value.allowed}
                  onChange={(e) => block.edit((g) => ({ ...g, allowed: e.target.value }))}
                />
                <p id={`${id}-allowed-hint`} className="text-sm text-muted">
                  GitHub logins whose requests are listed, one per line.
                </p>
              </div>
              <div className="grid content-start gap-1">
                <label htmlFor={`${id}-bots`} className="text-sm font-semibold">
                  Bots
                </label>
                <Textarea
                  id={`${id}-bots`}
                  aria-describedby={`${id}-bots-hint`}
                  className="font-mono"
                  value={block.value.bots}
                  onChange={(e) => block.edit((g) => ({ ...g, bots: e.target.value }))}
                />
                <p id={`${id}-bots-hint`} className="text-sm text-muted">
                  Bot logins whose messages may become candidates, one per line.
                </p>
              </div>
            </div>
          </Block>
          <Block title="Admission">
            <p className="max-w-prose text-sm text-muted">
              Listed senders are on the allowed list; an addressed message mentions or assigns Athena.
            </p>
            <AdmissionTable
              layout={layout}
              value={block.value.admission}
              onChange={(admission) => block.edit((g) => ({ ...g, admission }))}
            />
          </Block>
          <SaveBar label="GitHub" {...block.bar} />
        </>
      )}
    </Section>
  );
}
