// Settings › Autopilot: the owner's standing instructions, each work Athena starts on its own in a GitHub repository,
// on a clock or when the repository changes. Saved with the rest of the Policy (setPolicy), against the revision the
// owner edited from. The cron expression is checked here only for its shape; the server says whether it is valid.
import { PlusIcon, Trash2Icon } from "lucide-react";
import { useCallback, useId, useLayoutEffect, useMemo, useRef } from "react";
import { Link } from "react-router";
import { useAthena } from "@/lib/store/state";
import { Waiting } from "@/ui/athena/Waiting";
import { Button } from "@/ui/primitives/button";
import { Input } from "@/ui/primitives/input";
import { Select } from "@/ui/primitives/select";
import { Switch } from "@/ui/primitives/switch";
import { Textarea } from "@/ui/primitives/textarea";
import {
  autopilotSlug,
  CRON_PRESETS,
  DEFAULT_CI_RED_HOURS,
  type Entry,
  equalAutopilot,
  MAX_CI_RED_HOURS,
  readAutopilot,
  validateAutopilot,
  WHENS,
  type When,
  writeAutopilot,
} from "./autopilot";
import { isObject, str } from "./policy";
import { savePolicy, usePolicyDoc } from "./PolicyForm";
import { SaveBar, useSaveBlock } from "./SaveBar";
import { Block, Section } from "./Section";

const MODE_LINE: Record<string, string> = {
  off: "Accept mode is off, so these entries do nothing yet.",
  observe: "Accept mode is observe, so Athena only records what it would have started.",
};

export function Autopilot() {
  const { doc, revision, ready } = usePolicyDoc();
  const all = useAthena((s) => s.repos);
  const repos = useMemo(() => all.filter((r) => r.kind === "github").map((r) => r.name), [all]);
  const server = useMemo(() => (doc ? readAutopilot(doc) : undefined), [doc]);
  const send = useCallback((e: Entry[], base: number) => savePolicy(base, (d) => writeAutopilot(d, e)), []);
  const block = useSaveBlock({ label: "Autopilot", server, revision, equal: equalAutopilot, validate: validateAutopilot, send });
  // A Policy that never set a mode is off.
  const mode = doc && isObject(doc) ? str(doc.mode) || "off" : "off";
  const modeLine = MODE_LINE[mode];

  return (
    <Section title="Autopilot" text="Work Athena starts on its own, on a clock or when a repository changes.">
      {!ready || !block.value ? (
        <Waiting className="px-4 py-6 lg:px-6" />
      ) : (
        <>
          <Block title="Entries">
            {modeLine && <p className="max-w-prose text-sm font-medium">{modeLine}</p>}
            <Entries entries={block.value} repos={repos} onChange={(next) => block.edit(next)} />
          </Block>
          <SaveBar label="Autopilot" {...block.bar} />
        </>
      )}
    </Section>
  );
}

/** Where the keyboard goes once the entries re-render: the next entry's name after a removal, else Add entry. */
type FocusTarget = { key: string } | { key: null };

function Entries({ entries, repos, onChange }: { entries: Entry[]; repos: string[]; onChange(entries: Entry[]): void }) {
  const next = useRef(0);
  const list = useRef<HTMLDivElement>(null);
  const focusAfter = useRef<FocusTarget | null>(null);
  useLayoutEffect(() => {
    const t = focusAfter.current;
    focusAfter.current = null;
    if (!t || !list.current) return;
    const sel = t.key === null ? '[data-control="add"]' : `[data-entry="${CSS.escape(t.key)}"] [data-control="name"]`;
    list.current.querySelector<HTMLElement>(sel)?.focus();
  }, [entries]);

  const set = (i: number, patch: Partial<Entry>) => onChange(entries.map((e, j) => (j === i ? { ...e, ...patch } : e)));
  const remove = (i: number) => {
    const after = entries[i + 1];
    focusAfter.current = after ? { key: after.key } : { key: null };
    onChange(entries.filter((_, j) => j !== i));
  };
  return (
    <div ref={list} className="grid gap-3">
      {entries.length > 0 && (
        <ol className="grid gap-3">
          {entries.map((e, i) => (
            <EntryEditor key={e.key} entry={e} index={i} repos={repos} onChange={(p) => set(i, p)} onRemove={() => remove(i)} />
          ))}
        </ol>
      )}
      <div>
        <Button
          data-control="add"
          onClick={() =>
            onChange([
              ...entries,
              {
                key: `n${++next.current}`,
                name: "",
                instructions: "",
                repo: repos[0] ?? "",
                enabled: true,
                when: "cron",
                cron: "",
                ciRedHours: DEFAULT_CI_RED_HOURS,
                orig: {},
              },
            ])
          }
        >
          <PlusIcon aria-hidden />
          Add entry
        </Button>
      </div>
    </div>
  );
}

function EntryEditor({
  entry,
  index,
  repos,
  onChange,
  onRemove,
}: {
  entry: Entry;
  index: number;
  repos: string[];
  onChange(patch: Partial<Entry>): void;
  onRemove(): void;
}) {
  const id = useId();
  const called = entry.name.trim() || `entry ${index + 1}`;
  const options = entry.repo && !repos.includes(entry.repo) ? [entry.repo, ...repos] : repos;
  const preset = CRON_PRESETS.find((p) => p.cron === entry.cron.trim())?.cron ?? "";
  // Only an entry the server has can have runs, under the name it has there.
  const saved = Object.keys(entry.orig).length > 0 ? autopilotSlug(str(entry.orig.name)) : "";
  return (
    <li data-entry={entry.key} className="grid min-w-0 gap-3 rounded-md border border-line-soft p-3">
      <div className="grid gap-1">
        <label htmlFor={`${id}-name`} className="text-sm font-semibold">
          Name
        </label>
        <Input
          id={`${id}-name`}
          data-control="name"
          value={entry.name}
          aria-invalid={entry.name.trim() === "" || undefined}
          onChange={(e) => onChange({ name: e.target.value })}
        />
      </div>
      <div className="grid gap-1">
        <label htmlFor={`${id}-instructions`} className="text-sm font-semibold">
          Instructions
        </label>
        <Textarea id={`${id}-instructions`} value={entry.instructions} onChange={(e) => onChange({ instructions: e.target.value })} />
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <div className="grid min-w-0 content-start gap-1">
          <label htmlFor={`${id}-repo`} className="text-sm font-semibold">
            Repository
          </label>
          {options.length === 0 ? (
            <p className="text-sm text-muted">No GitHub repository is known yet.</p>
          ) : (
            <Select id={`${id}-repo`} value={entry.repo} onChange={(e) => onChange({ repo: e.target.value })}>
              {entry.repo === "" && <option value="" />}
              {options.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </Select>
          )}
        </div>
        <div className="grid min-w-0 content-start gap-1">
          <label htmlFor={`${id}-when`} className="text-sm font-semibold">
            Trigger
          </label>
          <Select id={`${id}-when`} value={entry.when} onChange={(e) => onChange({ when: e.target.value as When })}>
            {WHENS.map((w) => (
              <option key={w.value} value={w.value}>
                {w.label}
              </option>
            ))}
          </Select>
        </div>
      </div>
      {entry.when === "cron" && (
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="grid min-w-0 content-start gap-1">
            <label htmlFor={`${id}-cron`} className="text-sm font-semibold">
              Schedule
            </label>
            <Input
              id={`${id}-cron`}
              className="font-mono"
              aria-describedby={`${id}-cron-hint`}
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              value={entry.cron}
              onChange={(e) => onChange({ cron: e.target.value })}
            />
          </div>
          <div className="grid min-w-0 content-start gap-1">
            <label htmlFor={`${id}-preset`} className="text-sm font-semibold">
              Preset
            </label>
            <Select id={`${id}-preset`} value={preset} onChange={(e) => e.target.value && onChange({ cron: e.target.value })}>
              <option value="">Custom</option>
              {CRON_PRESETS.map((p) => (
                <option key={p.cron} value={p.cron}>
                  {p.label}
                </option>
              ))}
            </Select>
          </div>
          <p id={`${id}-cron-hint`} className="max-w-prose text-sm text-muted sm:col-span-2">
            Minute, hour, day, month, weekday, in this server's local time. At least 15 minutes apart.
          </p>
        </div>
      )}
      {entry.when === "ci_red" && (
        <div className="grid max-w-xs gap-1">
          <label htmlFor={`${id}-hours`} className="text-sm font-semibold">
            Hours
          </label>
          <Input
            id={`${id}-hours`}
            type="number"
            inputMode="numeric"
            min={1}
            max={MAX_CI_RED_HOURS}
            aria-describedby={`${id}-hours-hint`}
            value={Number.isNaN(entry.ciRedHours) ? "" : entry.ciRedHours}
            onChange={(e) => onChange({ ciRedHours: e.target.value === "" ? Number.NaN : Number(e.target.value) })}
          />
          <p id={`${id}-hours-hint`} className="text-sm text-muted">
            How long the default branch stays red before this starts.
          </p>
        </div>
      )}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <label className="flex min-h-11 items-center gap-2 text-sm lg:min-h-0">
          <Switch checked={entry.enabled} onCheckedChange={(enabled) => onChange({ enabled })} aria-label={`${called} enabled`} />
          <span aria-hidden>Enabled</span>
        </label>
        {saved && (
          <Link
            to={`/routing?q=schedule:${saved}`}
            className="inline-flex min-h-11 items-center text-sm font-medium underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-0"
          >
            See its runs
          </Link>
        )}
        <Button className="ml-auto" size="icon" variant="ghost" onClick={onRemove} aria-label={`Remove ${called}`}>
          <Trash2Icon aria-hidden />
        </Button>
      </div>
    </li>
  );
}
