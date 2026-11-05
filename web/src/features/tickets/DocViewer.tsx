// A Ticket's file, read in the page: a panel over the Ticket on the desktop (Esc or Close), the whole screen with Back on
// the phone. A lazy chunk. What it shows follows the file's kind (lib/artifacts/files.ts): Markdown rendered by the
// safe renderer (its source a toggle away), code and text as numbered lines (JSON pretty-printed, a small CSV as a
// table), images inline. SVG and HTML are only ever their source: nothing here renders markup, and no file is put in
// a frame. A PDF, a binary and an image too big to view are offered as Downloads; every file keeps its Download.
//
// Text comes from the file's ?view=1 route (text/plain, its first 1 MiB); an image's <img> loads that route itself.
import { ArrowLeftIcon, DownloadIcon, XIcon } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { Link, useLocation } from "react-router";
import {
  type Artifact,
  fileHref,
  type FileText,
  getText,
  type TicketFile,
  type TicketFiles,
  useArtifact,
  viewHref,
} from "@/lib/artifacts/api";
import { baseName, fileKind, resolveFile } from "@/lib/artifacts/files";
import { hasBidi, markBidi } from "@/lib/text/ansi";
import { Markdown } from "@/lib/text/markdown";
import { cn } from "@/ui/cn";
import { EmptyState } from "@/ui/athena/EmptyState";
import { ErrorState } from "@/ui/athena/ErrorState";
import { Mono } from "@/ui/athena/Mono";
import { Button, buttonVariants } from "@/ui/primitives/button";
import { Dialog, DialogDescription, DialogTitle, Modal } from "@/ui/primitives/dialog";
import { Skeleton } from "@/ui/primitives/skeleton";
import { ToggleGroup, ToggleGroupItem } from "@/ui/primitives/toggle-group";
import { BidiWarning } from "./BidiWarning";
import { looksBinary, parseCsv, prettyJson, textLines } from "./docText";
import { allFilesTo } from "./fileLink";
import { FILE_STATUS, FileIconFor, formatSize } from "./FilesView";

/** This chunk's sentinel: the bundle budget fails if it is in the initial route (scripts/bundle-budget.mjs). */
export const LAZY_SENTINEL = "athena-lazy-chunk:doc-viewer";

/** The view route serves an image whole up to this size (internal/web/artifacts.go maxViewImage). */
const MAX_IMAGE = 8 * 1024 * 1024;
/** A CSV of at most this many rows is a table. */
const CSV_ROWS = 200;
/** Numbered lines shown at a time. */
const LINES = 2000;

const GUTTER = ["w-[2ch]", "w-[2ch]", "w-[3ch]", "w-[4ch]", "w-[5ch]", "w-[6ch]", "w-[7ch]"];

/** The file's text as numbered lines: mono, wrapping (never sideways), scrolling with the panel. */
function CodeLines({ name, text }: { name: string; text: string }) {
  const lines = textLines(text);
  const [shown, setShown] = useState(LINES);
  const gutter = GUTTER[Math.min(String(lines.length).length, GUTTER.length - 1)];
  return (
    <div className="grid min-w-0 gap-2">
      <div
        role="region"
        tabIndex={0}
        aria-label={`Source of ${name}`}
        className="min-w-0 rounded-md border border-line-soft bg-surface-2 py-3 font-mono text-xs leading-[1.65] [tab-size:4]"
      >
        {lines.slice(0, shown).map((line, i) => (
          <div key={i} className="flex min-w-0 px-3">
            <span aria-hidden data-line-number={i + 1} className={cn("shrink-0 pr-4 text-right text-muted tabular-nums select-none", gutter)}>
              {i + 1}
            </span>
            <span dir="auto" className="min-w-0 flex-1 whitespace-pre-wrap wrap-anywhere">
              {line || " "}
            </span>
          </div>
        ))}
      </div>
      {lines.length > shown && (
        <Button size="sm" className="justify-self-start" onClick={() => setShown((n) => n + LINES)}>
          {`Show ${Math.min(LINES, lines.length - shown)} more lines`}
        </Button>
      )}
    </div>
  );
}

function CsvTable({ rows }: { rows: string[][] }) {
  const [head, ...body] = rows;
  return (
    <div tabIndex={0} role="group" aria-label="Table" className="max-w-full overflow-x-auto rounded-md border border-line-soft">
      <table className="min-w-full border-collapse text-[13px]">
        <thead className="bg-surface-2">
          <tr>
            {head.map((c, k) => (
              <th key={k} dir="auto" className="border-b border-line px-3 py-1.5 text-left font-semibold wrap-anywhere">
                {c}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {body.map((r, k) => (
            <tr key={k}>
              {r.map((c, j) => (
                <td key={j} dir="auto" className="border-b border-line-soft px-3 py-1.5 align-top whitespace-pre-wrap wrap-anywhere">
                  {c}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function DownloadLink({ href, name, className }: { href: string; name: string; className?: string }) {
  return (
    <a href={href} download={name} className={cn(buttonVariants({ size: "sm" }), className)}>
      <DownloadIcon aria-hidden />
      Download
    </a>
  );
}

/** A file shown by name and size only, with Download. */
function NoPreview({ title, detail, href, name, size }: { title: string; detail: string; href: string; name: string; size?: number }) {
  return (
    <div className="grid justify-items-start gap-3 rounded-md border border-line-soft px-5 py-6">
      <div className="flex min-w-0 items-center gap-2">
        <FileIconFor path={name} className="size-5" />
        <Mono tone="default" className="min-w-0 wrap-anywhere">
          {name}
        </Mono>
        {size !== undefined && <Mono className="shrink-0">{formatSize(size)}</Mono>}
      </div>
      <div className="grid gap-1">
        <h3 className="font-semibold">{title}</h3>
        <p className="max-w-prose text-muted">{detail}</p>
      </div>
      <DownloadLink href={href} name={name} />
    </div>
  );
}

type Mode = "rendered" | "source";

function TextBody({
  text: a,
  path,
  name,
  download,
  mode,
  onModes,
}: {
  text: Artifact<FileText>;
  path: string;
  name: string;
  download: string;
  mode: Mode;
  onModes(available: boolean): void;
}) {
  const kind = fileKind(path);
  const ready = a.status === "ready" ? a.data : undefined;
  const csv = ready && kind === "csv" && !ready.truncated ? parseCsv(ready.text, CSV_ROWS) : undefined;
  const table = !!csv && csv.length > 0;
  const toggles = kind === "markdown" || table;
  useEffect(() => onModes(toggles), [toggles, onModes]);

  if (a.status === "loading") {
    return (
      <div aria-busy="true" className="grid gap-2">
        <Skeleton className="h-5 w-1/2" />
        <Skeleton className="h-4 w-full" />
        <Skeleton className="h-4 w-5/6" />
        <Skeleton className="h-4 w-2/3" />
      </div>
    );
  }
  if (a.status === "error") {
    if (a.error.code === "not_found") {
      return <EmptyState title="This file is not there any more." description="It may have been moved or deleted since the listing was read." />;
    }
    if (a.error.code === "unauthenticated") return <ErrorState title="You were signed out." detail="Sign in again to see this." />;
    return (
      <ErrorState
        title="The file could not be loaded."
        detail={a.error.code === "network" ? "No answer from Athena. Check your connection." : a.error.code}
        onRetry={a.retry}
      />
    );
  }
  const { text } = a.data;
  if (kind === "other" && looksBinary(text)) {
    return (
      <NoPreview
        title="No preview for this file."
        detail="It is not text. Download it to open it on this device."
        href={download}
        name={name}
        size={a.data.size}
      />
    );
  }
  if (mode === "rendered" && kind === "markdown") {
    return (
      <article className="mx-auto w-full max-w-[72ch]">
        <Markdown text={text} document />
      </article>
    );
  }
  if (mode === "rendered" && table) return <CsvTable rows={csv} />;
  const pretty = kind === "json" && !a.data.truncated ? prettyJson(text) : undefined;
  return (
    <div className="grid min-w-0 gap-3">
      {kind === "source" && (
        <p role="note" className="text-sm text-muted">
          Shown as source: SVG and HTML files are never rendered here.
        </p>
      )}
      <CodeLines name={name} text={pretty ?? text} />
    </div>
  );
}

function Body({ ticketId, entry, mode, onModes }: { ticketId: string; entry: Pick<TicketFile, "path" | "size">; mode: Mode; onModes(a: boolean): void }) {
  const kind = fileKind(entry.path);
  const name = markBidi(baseName(entry.path));
  const download = fileHref(ticketId, entry.path);
  const reads = kind !== "image" && kind !== "pdf";
  const text = useArtifact<FileText>(reads ? viewHref(ticketId, entry.path) : "", 0, getText);
  const [broken, setBroken] = useState(false);
  // An image or a PDF has no Rendered / Source toggle (a text body says whether it has one).
  useEffect(() => {
    if (!reads) onModes(false);
  }, [reads, onModes]);
  const truncated = text.status === "ready" && text.data.truncated;
  const size = text.status === "ready" ? text.data.size : entry.size;

  let body;
  if (kind === "pdf") {
    body = <NoPreview title="PDF files open with Download." detail="The dashboard does not show PDFs inline." href={download} name={name} size={entry.size} />;
  } else if (kind === "image" && (entry.size > MAX_IMAGE || broken)) {
    body = (
      <NoPreview
        title={broken ? "This image could not be shown." : "This image is too large to show here."}
        detail={broken ? "Download it to open it on this device." : `Images up to ${formatSize(MAX_IMAGE)} show inline. Download it to see it.`}
        href={download}
        name={name}
        size={entry.size}
      />
    );
  } else if (kind === "image") {
    body = (
      <div className="grid place-items-center rounded-md border border-line-soft bg-surface-2 p-4">
        <img src={viewHref(ticketId, entry.path)} alt={name} onError={() => setBroken(true)} className="h-auto max-h-[75dvh] max-w-full object-contain" />
      </div>
    );
  } else {
    body = <TextBody text={text} path={entry.path} name={name} download={download} mode={mode} onModes={onModes} />;
  }
  return (
    <div className="grid min-w-0 gap-4">
      {truncated && (
        <p role="note" className="rounded-xs border border-line-soft bg-surface-2 px-3 py-2 text-sm">
          {"Showing the first 1 MB. "}
          <a href={download} download={name} className="font-medium underline decoration-line-soft underline-offset-4 hover:decoration-text">
            Download the whole file
          </a>
          {` (${formatSize(size)}).`}
        </p>
      )}
      {body}
    </div>
  );
}

export default function DocViewer({
  ticketId,
  file,
  files,
  dir,
  onClose,
  onResolve,
}: {
  ticketId: string;
  /** The file asked for (?file=): a listed path, or an absolute one from the session. */
  file: string;
  files: Artifact<TicketFiles>;
  /** The Ticket's directory, for an absolute path in it. */
  dir: string;
  onClose(): void;
  /** Called with the listed path an absolute file names, to put in the URL instead. */
  onResolve(path: string): void;
}) {
  const here = useLocation();
  const [mode, setMode] = useState<Mode>("rendered");
  const [modes, setModes] = useState(false);
  // Only a path in the loaded listing is read: ?file= is a URL anyone can craft, and never a way out of the Ticket.
  const listed = files.status === "ready" ? files.data.files : undefined;
  const path = listed ? resolveFile(file, listed, dir) : undefined;
  const entry = path ? listed?.find((f) => f.path === path) : undefined;

  useEffect(() => {
    if (path && path !== file) onResolve(path);
  }, [path, file, onResolve]);
  // A file written after the listing was read: read it once more before saying the file is not there.
  const reloaded = useRef(false);
  const unresolved = files.status === "ready" && !path;
  useEffect(() => {
    if (unresolved && !reloaded.current && files.reload) {
      reloaded.current = true;
      files.reload();
    }
  }, [unresolved, files]);

  const shownPath = path ?? file;
  const name = markBidi(baseName(shownPath));
  const folder = markBidi(shownPath.slice(0, shownPath.length - baseName(shownPath).length));
  const status = entry?.status ? FILE_STATUS[entry.status] : undefined;
  const meta = [folder || undefined, entry && entry.size > 0 ? formatSize(entry.size) : undefined, status?.label].filter(Boolean).join(" · ");

  let content;
  if (entry) {
    content = <Body key={entry.path} ticketId={ticketId} entry={entry} mode={mode} onModes={setModes} />;
  } else if (unresolved) {
    content = (
      <EmptyState
        title="This file is not among the Ticket's files."
        description="Only files in the Ticket's directory, or that its worktree added or changed, can be read here."
        action={
          <Button asChild>
            <Link to={allFilesTo(here)}>All files</Link>
          </Button>
        }
      />
    );
  } else if (files.status === "error") {
    content = (
      <ErrorState
        title="The files could not be loaded."
        detail={files.error.code === "not_found" ? "This Ticket has no files to read right now." : files.error.code}
        onRetry={files.retry}
      />
    );
  } else {
    content = (
      <div aria-busy="true" className="grid gap-2">
        <Skeleton className="h-5 w-1/2" />
        <Skeleton className="h-4 w-full" />
        <Skeleton className="h-4 w-5/6" />
      </div>
    );
  }

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <Modal
        hideClose
        data-chunk={LAZY_SENTINEL}
        className={cn(
          "m-0 h-dvh max-h-none w-full max-w-none border-0 open:animate-fade-in",
          "lg:my-0 lg:mr-0 lg:ml-auto lg:w-[min(64rem,62vw)] lg:border-l lg:border-line lg:open:animate-slide-in",
        )}
        bodyClassName="flex h-full min-h-0 flex-col"
      >
        <header className="flex flex-wrap items-start gap-x-3 gap-y-2 border-b border-line-soft px-4 pt-[max(0.75rem,env(safe-area-inset-top))] pb-3 lg:flex-nowrap lg:px-6 lg:py-4">
          <Button variant="ghost" size="sm" onClick={onClose} className="-ml-2 lg:hidden">
            <ArrowLeftIcon aria-hidden />
            Back
          </Button>
          <div className="order-3 grid min-w-0 basis-full gap-1 lg:order-none lg:flex-1 lg:basis-auto">
            <DialogTitle
              tabIndex={-1}
              data-autofocus
              className="flex min-w-0 items-center gap-2 text-lg font-semibold wrap-anywhere focus:outline-none"
            >
              <FileIconFor path={shownPath} className="size-5" />
              <span className="min-w-0">{name}</span>
            </DialogTitle>
            <DialogDescription className="font-mono text-xs wrap-anywhere">{meta || "File"}</DialogDescription>
            {hasBidi(shownPath) && <BidiWarning />}
          </div>
          <div className="order-2 ml-auto flex shrink-0 items-center gap-2 lg:order-none">
            {modes && (
              <ToggleGroup type="single" value={mode} onValueChange={(v) => v && setMode(v as Mode)} aria-label="Show as">
                <ToggleGroupItem value="rendered">{fileKind(shownPath) === "csv" ? "Table" : "Rendered"}</ToggleGroupItem>
                <ToggleGroupItem value="source">Source</ToggleGroupItem>
              </ToggleGroup>
            )}
            {entry && <DownloadLink href={fileHref(ticketId, entry.path)} name={name} />}
            <Button variant="ghost" size="icon" aria-label="Close" onClick={onClose} className="hidden lg:inline-flex">
              <XIcon aria-hidden />
            </Button>
          </div>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain px-4 pt-5 pb-[max(1.5rem,env(safe-area-inset-bottom))] lg:px-8 lg:pt-6">
          {content}
        </div>
      </Modal>
    </Dialog>
  );
}
