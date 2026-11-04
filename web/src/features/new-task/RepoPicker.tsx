// The repository combobox of the New screen: the owner's repositories grouped by kind (GitHub, Local repositories,
// Folders), "No repository" (the default, an empty field), or free entry of owner/name (a GitHub repository) or an
// absolute path (a directory). The field's text is the value; resolveRepo says what it means and whether it is valid.
import { Command as CommandPrimitive } from "cmdk";
import { useId, useState } from "react";
import type { RepoView } from "@/gen/athena/v1/state_pb";
import { cn } from "@/ui/cn";
import { Command, CommandEmpty, CommandGroup, CommandItem, CommandList } from "@/ui/primitives/command";
import { fieldClass } from "@/ui/primitives/field";

export const OWNER_NAME = /^[A-Za-z0-9-]+\/[A-Za-z0-9._-]+$/;

export type RepoChoice =
  | { kind: "none" }
  | { kind: "repo"; repo: string }
  | { kind: "dir"; dir: string }
  | { kind: "invalid" };

/** What the field's text means. A listed repository goes by its kind: GitHub as repo, the rest as dir. */
export function resolveRepo(text: string, repos: readonly RepoView[]): RepoChoice {
  const t = text.trim();
  if (t === "") return { kind: "none" };
  const listed = repos.find((r) => r.name === t);
  if (listed) return listed.kind === "github" ? { kind: "repo", repo: t } : { kind: "dir", dir: t };
  if (OWNER_NAME.test(t)) return { kind: "repo", repo: t };
  if (t.startsWith("/")) return { kind: "dir", dir: t };
  return { kind: "invalid" };
}

const GROUPS: ReadonlyArray<{ kind: string; label: string }> = [
  { kind: "github", label: "GitHub" },
  { kind: "git", label: "Local repositories" },
  { kind: "dir", label: "Folders" },
];

export const REPO_ERROR = "Use owner/name for a GitHub repository, or an absolute path starting with /.";

export function RepoPicker({
  value,
  onChange,
  repos,
}: {
  value: string;
  onChange(text: string): void;
  repos: readonly RepoView[];
}) {
  const [open, setOpen] = useState(false);
  const errorId = useId();
  const choice = resolveRepo(value, repos);
  const invalid = choice.kind === "invalid";
  const typed = value.trim();
  // Free entry offers itself first, so Enter takes what was typed rather than a similar listed name.
  // A name already picked shows the whole list again, so the owner can switch.
  const picked = repos.some((r) => r.name === typed);
  const offerTyped = (choice.kind === "repo" || choice.kind === "dir") && !picked;
  const q = picked ? "" : typed.toLowerCase();
  const shown = (kind: string) => repos.filter((r) => r.kind === kind && (q === "" || r.name.toLowerCase().includes(q)));
  const pick = (text: string) => {
    onChange(text);
    setOpen(false);
  };

  return (
    <div className="relative grid min-w-0 gap-1">
      <Command
        shouldFilter={false}
        label="Repository"
        onKeyDown={(e) => {
          if (e.key === "Escape" && open) {
            e.stopPropagation();
            setOpen(false);
          }
        }}
        onBlur={(e) => {
          if (!e.currentTarget.contains(e.relatedTarget)) setOpen(false);
        }}
        className="size-auto overflow-visible bg-transparent"
      >
        <CommandPrimitive.Input
          aria-label="Repository"
          aria-invalid={invalid || undefined}
          aria-describedby={invalid ? errorId : undefined}
          placeholder="No repository"
          value={value}
          onValueChange={(v) => {
            onChange(v);
            setOpen(true);
          }}
          onFocus={() => setOpen(true)}
          onClick={() => setOpen(true)}
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          className={cn(fieldClass, "h-11 px-3 font-mono text-sm lg:h-9")}
        />
        {/* Always mounted, so the field's aria-controls names an element. Held down on the list, the field keeps
            focus, so a click picks before the blur closes it. */}
        <div
            hidden={!open}
            onMouseDown={(e) => e.preventDefault()}
            className="absolute top-full right-0 left-0 z-20 mt-1 rounded-md border border-line bg-surface"
          >
            <CommandList>
              {offerTyped && (
                <CommandItem value={`use ${typed}`} onSelect={() => pick(typed)}>
                  <span className="min-w-0 break-all">
                    Use <span className="font-mono">{typed}</span>
                  </span>
                </CommandItem>
              )}
              {typed === "" && (
                <CommandItem value="no repository" onSelect={() => pick("")}>
                  No repository
                </CommandItem>
              )}
              {GROUPS.map(({ kind, label }) => {
                const list = shown(kind);
                return list.length === 0 ? null : (
                  <CommandGroup key={kind} heading={label}>
                    {list.map((r) => (
                      <CommandItem key={r.name} value={`${kind} ${r.name}`} onSelect={() => pick(r.name)}>
                        <span className="min-w-0 font-mono text-sm break-all">{r.name}</span>
                      </CommandItem>
                    ))}
                  </CommandGroup>
                );
              })}
              {!offerTyped && typed !== "" && GROUPS.every((g) => shown(g.kind).length === 0) && (
                <CommandEmpty>No repository matches.</CommandEmpty>
              )}
            </CommandList>
          </div>
      </Command>
      {invalid && (
        <p id={errorId} role="alert" className="text-sm text-danger">
          {REPO_ERROR}
        </p>
      )}
    </div>
  );
}
