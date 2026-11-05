// Settings › Prompts (a lazy chunk): the prompt parts, each marked Edited when the owner overrode it and "Default
// updated" when its default changed under the override; the part's editor (Save sends setPrompt against the
// PromptsView revision, Reset returns it to the default after a confirmation); the old and new defaults compared
// above the owner's text, with Keep my text or Use the new default; the full prompts a new session gets; and the
// part's history. ?part=<name> picks the part.
import { timestampMs } from "@bufbuild/protobuf/wkt";
import { useCallback, useEffect, useId, useMemo, useState } from "react";
import { Link } from "react-router";
import type { PromptDetail } from "@/gen/athena/v1/commands_pb";
import { errorMessage, getPrompt, resetPrompt, setPrompt } from "@/lib/commands/actions";
import { selectSynced } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { Confirm } from "@/ui/athena/Confirm";
import { Mono } from "@/ui/athena/Mono";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { Waiting } from "@/ui/athena/Waiting";
import { cn } from "@/ui/cn";
import { Button } from "@/ui/primitives/button";
import { Textarea } from "@/ui/primitives/textarea";
import { toast } from "@/ui/primitives/toast";
import { useFilterParams } from "../tickets/Filters";
import { type BlockAction, SaveBar, useSaveBlock } from "./SaveBar";
import { Block, Section } from "./Section";

const EDITED = "Edited";
const DEFAULT_UPDATED = "Default updated";

export default function Prompts() {
  const { sp } = useFilterParams<"part">(["part"]);
  const view = useAthena((s) => s.prompts);
  const synced = useAthena(selectSynced);
  const parts = view?.parts ?? [];
  const chosen = parts.find((p) => p.name === sp.get("part")) ?? parts[0];

  return (
    <Section title="Prompts" text="The instructions every session starts with, part by part.">
      {!synced || !view ? (
        <Waiting className="px-4 py-6 lg:px-6" />
      ) : (
        <>
          <nav aria-label="Prompt parts" className="border-b border-line-soft px-4 py-3 lg:px-6">
            <ul className="flex flex-wrap gap-2">
              {parts.map((p) => (
                <li key={p.name}>
                  <Link
                    to={`?part=${encodeURIComponent(p.name)}`}
                    aria-current={p.name === chosen?.name ? "page" : undefined}
                    className={cn(
                      "inline-flex min-h-11 items-center gap-2 rounded-md border border-line-soft px-3 font-mono text-sm text-text hover:bg-surface-2 lg:min-h-9",
                      "aria-[current=page]:border-text aria-[current=page]:bg-text aria-[current=page]:text-bg",
                    )}
                  >
                    {p.name}
                    {p.overridden && <span className="font-sans text-xs">{EDITED}</span>}
                    {p.defaultUpdated && (
                      <span className="bg-accent px-1 font-sans text-xs font-semibold text-accent-fg">{DEFAULT_UPDATED}</span>
                    )}
                  </Link>
                </li>
              ))}
            </ul>
            <p className="mt-2 text-sm text-muted">Edits apply to sessions that start after you save.</p>
          </nav>
          {chosen && <PartEditor key={chosen.name} part={chosen.name} overridden={chosen.overridden} revision={view.revision} />}
        </>
      )}
    </Section>
  );
}

type Fetched = { detail: PromptDetail; revision: number };

/** The part's detail, asked for again whenever the prompts' revision moves (a save here or elsewhere). */
function usePromptDetail(part: string, revision: number): Fetched | undefined {
  const [fetched, setFetched] = useState<Fetched>();
  useEffect(() => {
    let live = true;
    void getPrompt(part).then((r) => {
      if (!live) return;
      if (r.ok && r.result?.case === "prompt") setFetched({ detail: r.result.value, revision });
      else if (!r.ok) toast({ title: "Prompt not loaded", description: errorMessage(r.code), tone: "danger" });
    });
    return () => {
      live = false;
    };
  }, [part, revision]);
  return fetched;
}

function PartEditor({ part, overridden, revision }: { part: string; overridden: boolean; revision: number }) {
  const id = useId();
  const fetched = usePromptDetail(part, revision);
  const detail = fetched?.detail;
  const server = detail ? detail.text || detail.defaultText : undefined;
  const send = useCallback((text: string, base: number) => setPrompt(part, base, text), [part]);
  const equal = useCallback((a: string, b: string) => a === b, []);
  const block = useSaveBlock({ label: part, server, revision: fetched?.revision, equal, send });

  /** A decision on the part, one at a time; a conflict goes to the save bar (Reload or Overwrite). */
  const decide = async (done: string, failed: string, action: BlockAction) => {
    const r = await block.act(action);
    if (!r || r.code === "conflict") return;
    if (r.ok) toast({ title: done });
    else toast({ title: failed, description: errorMessage(r.code), tone: "danger" });
  };
  const reset = () => decide("Reset to the default", "Not reset", (base) => resetPrompt(part, base));

  if (!fetched || !detail || block.value === undefined) return <Waiting className="px-4 py-6 lg:px-6" />;
  return (
    <>
      {!!detail.previousDefaultText && (
        <DefaultChanged
          before={detail.previousDefaultText}
          after={detail.defaultText}
          busy={block.busy}
          onKeep={() => void decide("Kept your text", "Not saved", (base) => setPrompt(part, base, detail.text))}
          onUseNew={() => void decide("Using the new default", "Not reset", (base) => resetPrompt(part, base))}
        />
      )}
      <Block title="Text">
        <label htmlFor={`${id}-text`} className="sr-only">
          {part}
        </label>
        <Textarea
          id={`${id}-text`}
          rows={16}
          spellCheck={false}
          className="max-h-[60dvh] min-h-48 font-mono text-sm"
          value={block.value}
          onChange={(e) => block.edit(e.target.value)}
        />
        {overridden && (
          <div>
            <Confirm
              size="sm"
              label="Reset"
              confirmLabel={`Reset ${part} to its default`}
              disabled={block.busy}
              onConfirm={() => void reset()}
            />
          </div>
        )}
      </Block>
      <SaveBar label={part} {...block.bar} />
      <Block title="Full prompt">
        <details className="grid gap-3">
          <summary className="min-h-11 cursor-pointer content-center text-sm font-semibold lg:min-h-0">
            Show the assembled worker and router prompts
          </summary>
          <FullPrompt title="Worker" text={detail.fullWorker} />
          <FullPrompt title="Router" text={detail.fullRouter} />
        </details>
      </Block>
      <Block title="History">
        {detail.history.length === 0 ? (
          <p className="text-sm text-muted">Never changed: the default.</p>
        ) : (
          <ol aria-label="History" className="grid">
            {detail.history.map((h) => (
              <li key={h.revision} className="flex min-w-0 flex-wrap gap-x-3 border-b border-line-soft py-2 text-sm">
                <Mono tone="default">{`#${h.revision}`}</Mono>
                <span>{h.reset ? "Reset to the default" : "Saved"}</span>
                <Mono>{h.origin}</Mono>
                <Mono className="ml-auto">
                  <RelativeTime at={h.at ? timestampMs(h.at) : Number.NaN} />
                </Mono>
              </li>
            ))}
          </ol>
        )}
      </Block>
    </>
  );
}

function FullPrompt({ title, text }: { title: string; text: string }) {
  return (
    <div className="mt-3 grid gap-1">
      <h4 className="text-sm font-semibold">{title}</h4>
      <pre className="max-h-96 overflow-y-auto rounded-md border border-line-soft bg-surface-2 p-3 font-mono text-xs break-words whitespace-pre-wrap">
        {text}
      </pre>
    </div>
  );
}

type Line = { kind: "same" | "removed" | "added"; text: string };

/** The line-by-line difference from a to b (longest common subsequence). */
export function diffLines(a: string, b: string): Line[] {
  const x = a.split("\n");
  const y = b.split("\n");
  const n = x.length;
  const m = y.length;
  const lcs: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--)
    for (let j = m - 1; j >= 0; j--) lcs[i][j] = x[i] === y[j] ? lcs[i + 1][j + 1] + 1 : Math.max(lcs[i + 1][j], lcs[i][j + 1]);
  const out: Line[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (x[i] === y[j]) {
      out.push({ kind: "same", text: x[i] });
      i++;
      j++;
    } else if (lcs[i + 1][j] >= lcs[i][j + 1]) out.push({ kind: "removed", text: x[i++] });
    else out.push({ kind: "added", text: y[j++] });
  }
  while (i < n) out.push({ kind: "removed", text: x[i++] });
  while (j < m) out.push({ kind: "added", text: y[j++] });
  return out;
}

function DefaultChanged({
  before,
  after,
  busy,
  onKeep,
  onUseNew,
}: {
  before: string;
  busy: boolean;
  after: string;
  onKeep(): void;
  onUseNew(): void;
}) {
  const id = useId();
  const lines = useMemo(() => diffLines(before, after), [before, after]);
  return (
    <section aria-labelledby={id} className="grid min-w-0 gap-3 border-b border-line-soft px-4 py-4 lg:px-6">
      <h3 id={id} className="font-semibold">
        The default changed
      </h3>
      <p className="max-w-prose text-sm text-muted">
        Your text was written for the old default. Lines marked − left it; lines marked + are new.
      </p>
      <pre className="max-h-80 overflow-y-auto rounded-md border border-line-soft bg-surface-2 p-3 font-mono text-xs break-words whitespace-pre-wrap">
        {lines.map((l, i) => (
          <div key={i} className="flex gap-2">
            <span aria-hidden className="w-3 shrink-0 text-muted">
              {l.kind === "removed" ? "−" : l.kind === "added" ? "+" : " "}
            </span>
            {l.kind === "removed" ? (
              <del className="min-w-0 text-muted">{l.text}</del>
            ) : l.kind === "added" ? (
              <ins className="min-w-0 font-semibold no-underline">{l.text}</ins>
            ) : (
              <span className="min-w-0">{l.text}</span>
            )}
          </div>
        ))}
      </pre>
      <div className="flex flex-wrap gap-2">
        <Button disabled={busy} onClick={onKeep}>
          Keep my text
        </Button>
        <Confirm
          label="Use the new default"
          confirmLabel="Replace my text with the new default"
          disabled={busy}
          onConfirm={onUseNew}
        />
      </div>
    </section>
  );
}
