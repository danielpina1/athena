// A Ticket's files: what its directory holds (a Ticket without a repository), or what its worktree added or changed
// (one with). Documents first, then the rest; each opens in the viewer (?file=) and keeps its own Download.
import { DownloadIcon, FileCodeIcon, FileIcon, FileTextIcon, ImageIcon, RefreshCwIcon } from "lucide-react";
import { Link, useLocation } from "react-router";
import { type Artifact, fileHref, type TicketFile, type TicketFiles } from "@/lib/artifacts/api";
import { documentsFirst, fileKind, isDocument } from "@/lib/artifacts/files";
import { hasBidi, markBidi } from "@/lib/text/ansi";
import { cn } from "@/ui/cn";
import { EmptyState } from "@/ui/athena/EmptyState";
import { Mono } from "@/ui/athena/Mono";
import { Button, buttonVariants } from "@/ui/primitives/button";
import { ArtifactFailure, ListSkeleton } from "./ArtifactFailure";
import { BidiWarning } from "./BidiWarning";
import { fileLinkTo } from "./fileLink";

/** 2.0 KB, 12 B. */
export function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 ** 3) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  return `${(bytes / 1024 ** 3).toFixed(1)} GB`;
}

/** How a repository Ticket's worktree has a file. */
export const FILE_STATUS: Record<string, { label: string; title: string }> = {
  "?": { label: "New", title: "New in the worktree, not committed" },
  A: { label: "Added", title: "Added on the Ticket's branch" },
  M: { label: "Changed", title: "Changed on the Ticket's branch" },
  R: { label: "Renamed", title: "Renamed on the Ticket's branch" },
  C: { label: "Copied", title: "Copied on the Ticket's branch" },
  T: { label: "Changed", title: "Its type changed on the Ticket's branch" },
};

export function FileIconFor({ path, className }: { path: string; className?: string }) {
  const kind = fileKind(path);
  const Icon = kind === "image" ? ImageIcon : kind === "markdown" || isDocument(path) ? FileTextIcon : kind === "other" || kind === "pdf" ? FileIcon : FileCodeIcon;
  return <Icon aria-hidden className={cn("size-4 shrink-0 text-muted", className)} />;
}

function Row({ ticketId, file }: { ticketId: string; file: TicketFile }) {
  const here = useLocation();
  // A name with bidirectional controls shows them as markers, here and as the saved name: never reordered.
  const bidi = hasBidi(file.path);
  const shown = bidi ? markBidi(file.path) : file.path;
  const status = file.status ? FILE_STATUS[file.status] : undefined;
  return (
    <li className="flex min-w-0 flex-wrap items-center gap-x-3 border-b border-line-soft">
      <Link
        to={fileLinkTo(here, file.path)}
        className="group flex min-h-11 min-w-0 flex-1 items-center gap-2 py-1 font-mono text-xs break-words wrap-anywhere hover:text-text lg:min-h-9"
      >
        <FileIconFor path={file.path} />
        <span className="min-w-0 underline decoration-line-soft underline-offset-4 group-hover:decoration-text">{shown}</span>
      </Link>
      {status && (
        <span title={status.title} className="shrink-0 text-xs text-muted">
          {status.label}
        </span>
      )}
      <Mono className="shrink-0">{formatSize(file.size)}</Mono>
      <a
        href={fileHref(ticketId, file.path)}
        download={shown.split("/").pop()}
        aria-label={`Download ${shown}`}
        title="Download"
        className={cn(buttonVariants({ variant: "ghost", size: "icon" }), "-mr-2 text-muted hover:text-text")}
      >
        <DownloadIcon aria-hidden />
      </a>
      {bidi && <BidiWarning className="mb-2 basis-full" />}
    </li>
  );
}

function Group({ label, ticketId, files }: { label: string; ticketId: string; files: TicketFile[] }) {
  if (files.length === 0) return null;
  return (
    <section className="grid min-w-0 gap-1">
      <h3 className="text-sm font-semibold">{label}</h3>
      <ul aria-label={label} className="grid min-w-0 border-t border-line-soft">
        {files.map((f) => (
          <Row key={f.path} ticketId={ticketId} file={f} />
        ))}
      </ul>
    </section>
  );
}

/** files is the Ticket's listing, read once by the Ticket's page (the Documents strip shares it). */
export function FilesView({ ticketId, files: a, repository }: { ticketId: string; files: Artifact<TicketFiles>; repository: boolean }) {
  if (a.status === "loading") return <ListSkeleton />;
  if (a.status === "error") {
    return <ArtifactFailure error={a.error} retry={a.retry} what="files" missing="No files to show." />;
  }
  const { files, truncated } = a.data;
  if (files.length === 0) {
    return repository ? (
      <EmptyState title="No new or changed files yet." description="Files the Ticket adds or changes in its worktree show up here to read." />
    ) : (
      <EmptyState title="No files yet." description="Files the session writes show up here to read." />
    );
  }
  const sorted = documentsFirst(files);
  const docs = sorted.filter((f) => isDocument(f.path));
  return (
    <div className="grid min-w-0 gap-5">
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
        <p className="min-w-0 flex-1 text-sm text-muted">
          {repository ? "What the Ticket added or changed in its worktree." : "What the Ticket's directory holds."}
        </p>
        {a.reload && (
          <Button variant="ghost" size="sm" onClick={a.reload}>
            <RefreshCwIcon aria-hidden />
            Refresh
          </Button>
        )}
      </div>
      {truncated && (
        <p role="note" className="rounded-xs border border-line-soft px-3 py-2 text-sm text-muted">
          This listing is incomplete: only the first files found are shown.
        </p>
      )}
      <Group label="Documents" ticketId={ticketId} files={docs} />
      <Group label="Other files" ticketId={ticketId} files={sorted.slice(docs.length)} />
    </div>
  );
}
