// The Ticket's newest Markdown documents (a report it was asked for), one tap from the top of its page: up to three,
// then All files. Nothing shows until the listing has some.
import { ArrowRightIcon, FileTextIcon } from "lucide-react";
import { Link, useLocation } from "react-router";
import type { Artifact, TicketFiles } from "@/lib/artifacts/api";
import { baseName, isMarkdown } from "@/lib/artifacts/files";
import { markBidi } from "@/lib/text/ansi";
import { allFilesTo, fileLinkTo } from "./fileLink";

const SHOWN = 3;

export function DocumentsStrip({ files }: { files: Artifact<TicketFiles> }) {
  const here = useLocation();
  if (files.status !== "ready") return null;
  const docs = files.data.files
    .filter((f) => isMarkdown(f.path))
    .sort((a, b) => Date.parse(b.modTime) - Date.parse(a.modTime))
    .slice(0, SHOWN);
  if (docs.length === 0) return null;
  const all = files.data.truncated ? "All files" : `All files (${files.data.files.length})`;
  return (
    <section aria-label="Documents" className="flex min-w-0 flex-wrap items-center gap-2 border-y border-line-soft py-2">
      <h2 className="mr-1 font-mono text-xs text-muted">Documents</h2>
      {docs.map((f) => (
        <Link
          key={f.path}
          to={fileLinkTo(here, f.path)}
          title={markBidi(f.path)}
          className="inline-flex min-h-11 max-w-full min-w-0 items-center gap-1.5 rounded-md border border-line-soft bg-surface px-2.5 text-sm font-medium hover:border-line hover:bg-surface-2 lg:min-h-8"
        >
          <FileTextIcon aria-hidden className="size-4 shrink-0 text-muted" />
          <span className="min-w-0 truncate">{markBidi(baseName(f.path))}</span>
        </Link>
      ))}
      <Link
        to={allFilesTo(here)}
        className="inline-flex min-h-11 items-center gap-1 px-1 text-sm text-muted underline decoration-line-soft underline-offset-4 hover:text-text hover:decoration-text lg:min-h-8"
      >
        {all}
        <ArrowRightIcon aria-hidden className="size-3.5" />
      </Link>
    </section>
  );
}
