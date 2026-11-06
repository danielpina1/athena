// One line per repository whose brake holds new work back: the owner has too much waiting on them there.
import type { RepoView } from "@/gen/athena/v1/state_pb";

export function brakeSentence(r: Pick<RepoView, "name" | "owed" | "ceiling" | "braked">): string {
  const items = r.braked === 1 ? "1 new item" : `${r.braked} new items`;
  return `Holding ${items} in ${r.name} until you catch up: ${r.owed} already wait on you (limit ${r.ceiling}).`;
}

export function BrakeNotice({ repos }: { repos: readonly RepoView[] }) {
  const braked = repos.filter((r) => r.braked > 0);
  if (braked.length === 0) return null;
  return (
    <ul aria-label="Held back" className="grid gap-2">
      {braked.map((r) => (
        <li key={r.name} className="border border-l-4 border-line border-l-accent bg-surface-2 px-3 py-2 text-sm break-words">
          {brakeSentence(r)}
        </li>
      ))}
    </ul>
  );
}
