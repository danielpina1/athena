// A Ticket's changes: the numstat file list, then the unified diff, file by file. A lazy chunk (the diff viewer is
// outside the initial bundle budget). Lines wrap and each block scrolls inside itself, so a 10,000-line diff or a
// 2,000-character line never moves the page sideways.
import { ChevronRightIcon } from "lucide-react";
import { useMemo, useRef, useState } from "react";
import { type Diff, type DiffFile, useArtifact } from "@/lib/artifacts/api";
import { cn } from "@/ui/cn";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Mono } from "@/ui/athena/Mono";
import { Button } from "@/ui/primitives/button";
import { ArtifactFailure, ListSkeleton } from "./ArtifactFailure";
import { BidiWarning } from "./BidiWarning";
import { type DiffLine, displayPath, type FileSection, parsePatch } from "./diff";

const STATUS: Record<string, string> = {
  A: "Added",
  M: "Modified",
  D: "Deleted",
  R: "Renamed",
  C: "Copied",
  T: "Type changed",
};

/** Files with at most this many lines start open, up to OPEN_FILES of them. */
const OPEN_LINES = 300;
const OPEN_FILES = 10;
/** A file shows this many lines at a time. */
const CHUNK = 1000;

const LINE: Record<DiffLine["kind"], { marker: string; row: string }> = {
  hunk: { marker: "", row: "border-transparent text-muted" },
  add: { marker: "+", row: "border-text bg-surface-2" },
  del: { marker: "−", row: "border-danger bg-surface-2 text-muted" },
  ctx: { marker: " ", row: "border-transparent" },
  note: { marker: "", row: "border-transparent text-muted italic" },
};

function Counts({ file }: { file: Pick<DiffFile, "added" | "deleted" | "binary"> }) {
  if (file.binary) return <Mono className="shrink-0">Binary</Mono>;
  return (
    <Mono className="shrink-0" tone="default">
      <span className="sr-only">{`${file.added} added, ${file.deleted} deleted`}</span>
      <span aria-hidden>{`+${file.added} −${file.deleted}`}</span>
    </Mono>
  );
}

function Lines({ lines }: { lines: DiffLine[] }) {
  const [shown, setShown] = useState(CHUNK);
  return (
    <>
      <div
        data-diff-block
        className="max-w-full overflow-x-auto rounded-xs border border-line-soft font-mono text-xs"
      >
        {lines.slice(0, shown).map((l, i) => (
          <div key={i} data-line={l.kind} className={cn("flex min-w-0 border-l-2", LINE[l.kind].row)}>
            <span aria-hidden className="w-4 shrink-0 text-center select-none">
              {LINE[l.kind].marker}
            </span>
            <span dir="auto" className="min-w-0 flex-1 pr-2 break-words wrap-anywhere whitespace-pre-wrap">
              {l.text}
            </span>
          </div>
        ))}
      </div>
      {lines.length > shown && (
        <Button size="sm" className="mt-2" onClick={() => setShown((n) => n + CHUNK)}>
          {`Show more lines (${lines.length - shown})`}
        </Button>
      )}
    </>
  );
}

function FileBlock({
  index,
  section,
  file,
  open,
  onToggle,
}: {
  index: number;
  section: FileSection;
  file?: DiffFile;
  open: boolean;
  onToggle(): void;
}) {
  const id = `diff-file-${index}`;
  return (
    <section id={id} aria-label={section.path || "File"} className="min-w-0 scroll-mt-4">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={`${id}-body`}
        onClick={onToggle}
        className="flex min-h-11 w-full min-w-0 items-center gap-2 border-b border-line text-left lg:min-h-9"
      >
        <ChevronRightIcon aria-hidden className={cn("size-4 shrink-0 transition-transform duration-(--motion-fast)", open && "rotate-90")} />
        <span dir="auto" className="min-w-0 flex-1 font-mono text-xs font-semibold break-words wrap-anywhere">
          {section.path}
        </span>
        {file && <Counts file={file} />}
      </button>
      {section.bidi && <BidiWarning className="mt-2" />}
      <div id={`${id}-body`} hidden={!open} className="pt-2">
        {open &&
          (section.lines.length > 0 ? (
            <Lines lines={section.lines} />
          ) : (
            <p className="text-sm text-muted">{section.binary ? "Binary file: no preview." : "No content changes."}</p>
          ))}
      </div>
    </section>
  );
}

function Changes({ diff }: { diff: Diff }) {
  const sections = useMemo(() => parsePatch(diff.patch), [diff.patch]);
  // The patch and the file list come from one git run, in one order; when the list was cut they differ and the
  // sections are matched by path.
  const fileFor = (s: FileSection, i: number) =>
    sections.length === diff.files.length ? diff.files[i] : diff.files.find((f) => displayPath(f.path) === s.path);
  const [open, setOpen] = useState<ReadonlySet<number>>(
    () => new Set(sections.flatMap((s, i) => (i < OPEN_FILES && s.lines.length <= OPEN_LINES ? [i] : []))),
  );
  const root = useRef<HTMLDivElement>(null);

  const toggle = (i: number) =>
    setOpen((prev) => {
      const next = new Set(prev);
      if (!next.delete(i)) next.add(i);
      return next;
    });
  const jump = (path: string, i: number) => {
    const at = sections.findIndex((s, j) => (sections.length === diff.files.length ? j === i : s.path === displayPath(path)));
    if (at < 0) return;
    setOpen((prev) => new Set(prev).add(at));
    requestAnimationFrame(() => root.current?.querySelector(`#diff-file-${at}`)?.scrollIntoView({ block: "start" }));
  };

  const added = diff.files.reduce((n, f) => n + f.added, 0);
  const deleted = diff.files.reduce((n, f) => n + f.deleted, 0);

  if (diff.files.length === 0 && diff.patch.trim() === "") {
    return (
      <EmptyState
        title="No changes yet."
        description={`${diff.branch || "The branch"} has nothing that its base does not.`}
      />
    );
  }
  return (
    <div ref={root} className="grid min-w-0 gap-4">
      {diff.truncated && (
        <p role="note" className="rounded-xs border border-line px-3 py-2 text-sm">
          This diff is too large to show in full: only the first part is shown.
        </p>
      )}
      <div className="grid gap-1">
        <Mono className="break-all">{diff.base ? `${diff.branch} against ${diff.base.slice(0, 7)}` : diff.branch}</Mono>
        <Mono tone="default">{`${diff.files.length} ${diff.files.length === 1 ? "file" : "files"}, +${added} −${deleted}`}</Mono>
      </div>
      <ul aria-label="Changed files" className="grid min-w-0 border-t border-line-soft">
        {diff.files.map((f, i) => (
          <li key={`${f.path}:${i}`} className="min-w-0 border-b border-line-soft">
            <button
              type="button"
              onClick={() => jump(f.path, i)}
              className="flex min-h-11 w-full min-w-0 items-center gap-3 text-left lg:min-h-9"
            >
              <Mono tone="default" className="w-4 shrink-0 text-center font-semibold">
                <span className="sr-only">{STATUS[f.status] ?? f.status}</span>
                <span aria-hidden>{f.status}</span>
              </Mono>
              <span dir="auto" className="min-w-0 flex-1 font-mono text-xs break-words wrap-anywhere">
                {f.oldPath ? `${displayPath(f.oldPath)} → ${displayPath(f.path)}` : displayPath(f.path)}
              </span>
              <Counts file={f} />
            </button>
          </li>
        ))}
      </ul>
      <div className="grid min-w-0 gap-4">
        {sections.map((s, i) => (
          <FileBlock key={`${s.path}:${i}`} index={i} section={s} file={fileFor(s, i)} open={open.has(i)} onToggle={() => toggle(i)} />
        ))}
      </div>
    </div>
  );
}

export default function DiffView({ ticketId }: { ticketId: string }) {
  const a = useArtifact<Diff>(`/api/tickets/${encodeURIComponent(ticketId)}/diff`);
  if (a.status === "loading") return <ListSkeleton />;
  if (a.status === "error") {
    return <ArtifactFailure error={a.error} retry={a.retry} what="changes" missing="No changes to show." />;
  }
  return <Changes diff={a.data} />;
}
