import { EmptyState } from "@/ui/athena/EmptyState";
import { ErrorState } from "@/ui/athena/ErrorState";
import type { ArtifactError } from "@/lib/artifacts/api";
import { Skeleton } from "@/ui/primitives/skeleton";

/** The shape of a list while it loads. */
export function ListSkeleton() {
  return (
    <div aria-busy="true" className="grid gap-3">
      <Skeleton className="h-5 w-1/3" />
      <Skeleton className="h-11 w-full" />
      <Skeleton className="h-11 w-full" />
      <Skeleton className="h-11 w-2/3" />
    </div>
  );
}

/**
 * Why a Ticket's diff or files did not load: signed out (sign-in is already showing), nothing there (`missing` says
 * what would be), or a failure that can be retried.
 */
export function ArtifactFailure({
  error,
  retry,
  what,
  missing,
}: {
  error: ArtifactError;
  retry(): void;
  /** "changes" or "files". */
  what: string;
  /** The sentence for a Ticket that has none: "No changes to show." */
  missing: string;
}) {
  if (error.code === "unauthenticated") {
    return <ErrorState title="You were signed out." detail="Sign in again to see this." />;
  }
  if (error.code === "not_found") {
    return <EmptyState title={missing} description={`This Ticket has no ${what} to read right now.`} />;
  }
  return (
    <ErrorState
      title={`The ${what} could not be loaded.`}
      detail={error.code === "network" ? "No answer from Athena. Check your connection." : error.code}
      onRetry={retry}
    />
  );
}
