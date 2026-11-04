import { timestampMs, type Timestamp } from "@bufbuild/protobuf/wkt";
import { Link } from "react-router";
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { safePrUrl } from "@/features/needs-you/ReviewCard";
import { RelativeTime } from "@/ui/athena/RelativeTime";
import { stageLabel } from "@/ui/athena/StageBadge";

const time = (ts: Timestamp | undefined) => (ts ? timestampMs(ts) : undefined);

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="font-mono text-xs text-muted">{label}</dt>
      <dd className="min-w-0 text-sm break-words wrap-anywhere">{children}</dd>
    </>
  );
}

/** What a Ticket is: where it works, its branch, model and runtime, when it started and ended, its pull request and Threads. */
export function Facts({ ticket, threads }: { ticket: TicketSummary; threads: readonly string[] }) {
  const created = time(ticket.createdAt);
  const updated = time(ticket.updatedAt);
  const ended = time(ticket.endedAt);
  const pr = safePrUrl(ticket.prUrl);
  return (
    <section aria-label="Facts">
      <dl className="grid grid-cols-[auto_minmax(0,1fr)] items-baseline gap-x-4 gap-y-1">
        {ticket.repo ? <Row label="Repository">{ticket.repo}</Row> : ticket.dir ? <Row label="Directory">{ticket.dir}</Row> : null}
        {ticket.branch && <Row label="Branch">{ticket.branch}</Row>}
        {ticket.model && <Row label="Model">{ticket.model}</Row>}
        {ticket.runtime && <Row label="Runtime">{ticket.runtime}</Row>}
        {created !== undefined && (
          <Row label="Created">
            <RelativeTime at={created} />
          </Row>
        )}
        {updated !== undefined && (
          <Row label="Updated">
            <RelativeTime at={updated} />
          </Row>
        )}
        {ticket.stage === "ended" && (
          <>
            {ended !== undefined && (
              <Row label="Ended">
                <RelativeTime at={ended} />
              </Row>
            )}
            <Row label="Outcome">{stageLabel({ stage: "ended", outcome: ticket.outcome })}</Row>
          </>
        )}
        {pr && (
          <Row label="Pull request">
            <a
              href={pr}
              target="_blank"
              rel="noopener"
              className="underline decoration-line-soft underline-offset-4 hover:decoration-text"
            >
              {pr.replace(/^https:\/\//, "")}
            </a>
          </Row>
        )}
        {threads.length > 0 && (
          <Row label="Threads">
            <ul className="grid gap-1">
              {threads.map((key) => (
                <li key={key} className="min-w-0">
                  <Link
                    to={`/routing?tab=threads&thread=${encodeURIComponent(key)}`}
                    className="inline-flex min-h-11 items-center font-mono text-xs break-all underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-0"
                  >
                    {key}
                  </Link>
                </li>
              ))}
            </ul>
          </Row>
        )}
      </dl>
    </section>
  );
}
