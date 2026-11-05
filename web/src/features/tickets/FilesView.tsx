import { type TicketFiles, useArtifact } from "@/lib/artifacts/api";
import { Mono } from "@/ui/athena/Mono";
import { EmptyState } from "@/ui/athena/EmptyState";
import { hasBidi, markBidi } from "@/lib/text/ansi";
import { ArtifactFailure, ListSkeleton } from "./ArtifactFailure";
import { BidiWarning } from "./BidiWarning";

/** 2.0 KB, 12 B. */
export function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  if (bytes < 1024 ** 3) return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  return `${(bytes / 1024 ** 3).toFixed(1)} GB`;
}

/** Each segment is escaped, the slashes between them are not: the route takes a path. */
const fileHref = (ticketId: string, path: string) =>
  `/api/tickets/${encodeURIComponent(ticketId)}/files/${path.split("/").map(encodeURIComponent).join("/")}`;

/** The files of a Ticket without a repository, each a download link. */
export function FilesView({ ticketId }: { ticketId: string }) {
  const a = useArtifact<TicketFiles>(`/api/tickets/${encodeURIComponent(ticketId)}/files`);
  if (a.status === "loading") return <ListSkeleton />;
  if (a.status === "error") {
    return <ArtifactFailure error={a.error} retry={a.retry} what="files" missing="No files to show." />;
  }
  const { files, truncated } = a.data;
  if (files.length === 0) {
    return <EmptyState title="No files yet." description="Files the session writes show up here to download." />;
  }
  return (
    <div className="grid min-w-0 gap-3">
      {truncated && (
        <p role="note" className="rounded-xs border border-line-soft px-3 py-2 text-sm text-muted">
          This listing is incomplete: only the first files found are shown.
        </p>
      )}
      <ul aria-label="Files" className="grid min-w-0 border-t border-line-soft">
        {files.map((f) => {
          // A name with bidirectional controls shows them as markers, here and as the saved name: never reordered.
          const bidi = hasBidi(f.path);
          const shown = bidi ? markBidi(f.path) : f.path;
          return (
            <li key={f.path} className="flex min-w-0 flex-wrap items-center gap-x-3 border-b border-line-soft">
              <a
                href={fileHref(ticketId, f.path)}
                download={shown.split("/").pop()}
                className="flex min-h-11 min-w-0 flex-1 items-center py-1 font-mono text-xs break-words wrap-anywhere underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-9"
              >
                {shown}
              </a>
              <Mono className="shrink-0">{formatSize(f.size)}</Mono>
              {bidi && <BidiWarning className="mb-2 basis-full" />}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
