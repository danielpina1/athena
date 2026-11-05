// A settings section's frame: its title and one sentence. On the phone the section is the page, so the title is the
// h1 and a back link returns to the list; on the desktop the list pane holds the h1 ("Settings") and the title is h2.
import { ArrowLeftIcon } from "lucide-react";
import type { ReactNode } from "react";
import { useId } from "react";
import { Link } from "react-router";
import { useLayout } from "@/app/Panes";

export function Section({ title, text, children }: { title: string; text: string; children: ReactNode }) {
  const phone = useLayout() === "phone";
  const id = useId();
  const Heading = phone ? "h1" : "h2";
  return (
    <section aria-labelledby={id} className="grid min-w-0 content-start">
      <header className="grid gap-1 border-b border-line p-4 lg:px-6">
        {phone && (
          <Link
            to="/settings"
            className="inline-flex min-h-11 items-center gap-1 self-start text-sm font-medium text-text underline decoration-line-soft underline-offset-4 hover:decoration-text"
          >
            <ArrowLeftIcon aria-hidden className="size-4" />
            Settings
          </Link>
        )}
        <Heading id={id} className="text-xl font-semibold break-words">
          {title}
        </Heading>
        <p className="max-w-prose text-muted">{text}</p>
      </header>
      {children}
    </section>
  );
}

/** A labelled group inside a section. */
export function Block({ title, children, id }: { title: string; children: ReactNode; id?: string }) {
  const own = useId();
  const hid = id ?? own;
  return (
    <section aria-labelledby={hid} className="grid min-w-0 gap-3 border-b border-line-soft px-4 py-4 lg:px-6">
      <h3 id={hid} className="font-mono text-xs tracking-wider text-muted uppercase">
        {title}
      </h3>
      {children}
    </section>
  );
}
