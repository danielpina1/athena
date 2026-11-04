// The command palette (⌘K, or the search button on the phone): go to any route; find a Ticket by id or title, a
// Trigger by its text, a Thread by its key ("#12", or a GitHub URL); act on the open Ticket (Reply, Stop, Retry,
// Interrupt, Watch session); start a new task; switch the theme. Reply asks for its text and Stop for a confirmation
// in a second step of the palette itself.
import {
  CornerDownLeftIcon,
  EyeIcon,
  HashIcon,
  OctagonXIcon,
  PauseIcon,
  PlusIcon,
  RotateCcwIcon,
  SunMoonIcon,
  TicketIcon,
  ZapIcon,
} from "lucide-react";
import { type KeyboardEvent, useMemo, useState } from "react";
import { useMatch, useNavigate } from "react-router";
import type { CommandResult } from "@/gen/athena/v1/commands_pb";
import type { TicketSummary, ThreadView, TriggerView } from "@/gen/athena/v1/state_pb";
import { NAV } from "@/app/routes";
import { errorMessage, interrupt, reply, retry, stop } from "@/lib/commands/actions";
import { useAthena } from "@/lib/store/state";
import { Mono } from "@/ui/athena/Mono";
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/ui/primitives/command";
import { toast } from "@/ui/primitives/toast";
import { useTheme } from "@/ui/theme";

/** At most this many search results per kind. */
const MAX_RESULTS = 20;

/**
 * What a Thread search looks for: a GitHub issue or pull request URL becomes its Thread key
 * (github:owner/name#n); anything else is matched as typed, ignoring case.
 */
export function threadQuery(q: string): string {
  const t = q.trim();
  const m = /^(?:https?:\/\/)?(?:www\.)?github\.com\/([^/\s]+)\/([^/\s]+)\/(?:pull|issues)\/(\d+)/i.exec(t);
  return m ? `github:${m[1].toLowerCase()}/${m[2].toLowerCase()}#${m[3]}` : t.toLowerCase();
}

function firstMatches<T>(values: Iterable<T>, keep: (v: T) => boolean): T[] {
  const out: T[] = [];
  for (const v of values) {
    if (out.length >= MAX_RESULTS) break;
    if (keep(v)) out.push(v);
  }
  return out;
}

type Results = { tickets: TicketSummary[]; triggers: TriggerView[]; threads: ThreadView[] };

function useResults(search: string): Results {
  const tickets = useAthena((s) => s.tickets);
  const triggers = useAthena((s) => s.triggers);
  const threads = useAthena((s) => s.threads);
  return useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return { tickets: [], triggers: [], threads: [] };
    const key = threadQuery(search);
    return {
      tickets: firstMatches(tickets.values(), (t) => t.id.toLowerCase().includes(q) || t.title.toLowerCase().includes(q)),
      triggers: q.length < 2 ? [] : firstMatches(triggers.values(), (t) => t.text.toLowerCase().includes(q)),
      threads: firstMatches(threads.values(), (t) => t.key.toLowerCase().includes(key)),
    };
  }, [search, tickets, triggers, threads]);
}

type Step = { kind: "root" } | { kind: "reply"; id: string } | { kind: "stop"; id: string };
const ROOT: Step = { kind: "root" };

/** Says how a command went: a short confirmation, or the error in plain words. */
async function report(done: string, failed: string, result: Promise<CommandResult>) {
  const r = await result;
  if (r.ok) toast({ title: done });
  else toast({ title: failed, description: errorMessage(r.code), tone: "danger" });
}

export function Palette({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const navigate = useNavigate();
  const selectedId = useMatch("/tickets/:id")?.params.id;
  const ticket = useAthena((s) => (selectedId ? s.tickets.get(selectedId) : undefined));
  const { resolved, setTheme } = useTheme();
  const [step, setStep] = useState<Step>(ROOT);
  const [search, setSearch] = useState("");
  const results = useResults(step.kind === "root" ? search : "");

  const setOpen = (next: boolean) => {
    if (!next) {
      setStep(ROOT);
      setSearch("");
    }
    onOpenChange(next);
  };
  const go = (to: string) => {
    setOpen(false);
    navigate(to);
  };
  const toStep = (next: Step) => {
    setStep(next);
    setSearch("");
  };
  // Backspace in an empty field leaves a step.
  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (step.kind !== "root" && e.key === "Backspace" && search === "") {
      e.preventDefault();
      toStep(ROOT);
    }
  };

  const placeholder =
    step.kind === "reply"
      ? `Reply to ${step.id}…`
      : step.kind === "stop"
        ? `Stop ${step.id}? Its session ends.`
        : "Search Tickets, Triggers, Threads or go to…";

  return (
    <CommandDialog
      open={open}
      onOpenChange={setOpen}
      commandProps={{ shouldFilter: step.kind === "root", loop: true, label: "Command palette" }}
    >
      <CommandInput value={search} onValueChange={setSearch} placeholder={placeholder} onKeyDown={onKeyDown} />
      <CommandList>
        {step.kind === "reply" && (
          <CommandGroup heading={`Reply to ${step.id}`}>
            <CommandItem
              value="send"
              disabled={search.trim() === ""}
              onSelect={() => {
                void report("Reply sent", "Reply not sent", reply(step.id, search.trim()));
                setOpen(false);
              }}
            >
              <CornerDownLeftIcon aria-hidden />
              Send reply
            </CommandItem>
          </CommandGroup>
        )}
        {step.kind === "stop" && (
          <CommandGroup heading={`Stop ${step.id}`}>
            <CommandItem
              value="confirm"
              className="text-danger"
              onSelect={() => {
                void report("Stopping", "Not stopped", stop(step.id, ""));
                setOpen(false);
              }}
            >
              <OctagonXIcon aria-hidden />
              {`Yes, stop ${step.id}`}
            </CommandItem>
            <CommandItem value="cancel" onSelect={() => toStep(ROOT)}>
              Cancel
            </CommandItem>
          </CommandGroup>
        )}
        {step.kind === "root" && (
          <>
            <CommandEmpty>Nothing matches.</CommandEmpty>
            {ticket && (
              <CommandGroup heading={ticket.id}>
                <CommandItem value={`reply ${ticket.id}`} onSelect={() => toStep({ kind: "reply", id: ticket.id })}>
                  <CornerDownLeftIcon aria-hidden />
                  {`Reply to ${ticket.id}`}
                </CommandItem>
                {ticket.stage !== "ended" && (
                  <CommandItem value={`stop ${ticket.id}`} onSelect={() => toStep({ kind: "stop", id: ticket.id })}>
                    <OctagonXIcon aria-hidden />
                    {`Stop ${ticket.id}`}
                  </CommandItem>
                )}
                {ticket.canRetry && (
                  <CommandItem
                    value={`retry ${ticket.id}`}
                    onSelect={() => {
                      void report("Retrying", "Not retried", retry(ticket.id));
                      setOpen(false);
                    }}
                  >
                    <RotateCcwIcon aria-hidden />
                    {`Retry ${ticket.id}`}
                  </CommandItem>
                )}
                {ticket.stage === "working" && (
                  <CommandItem
                    value={`interrupt ${ticket.id}`}
                    onSelect={() => {
                      void report("Interrupted", "Not interrupted", interrupt(ticket.id));
                      setOpen(false);
                    }}
                  >
                    <PauseIcon aria-hidden />
                    {`Interrupt ${ticket.id}`}
                  </CommandItem>
                )}
                <CommandItem value={`watch session ${ticket.id}`} onSelect={() => go(`/tickets/${ticket.id}?tab=session`)}>
                  <EyeIcon aria-hidden />
                  Watch session
                </CommandItem>
              </CommandGroup>
            )}
            {results.tickets.length > 0 && (
              <CommandGroup heading="Tickets" forceMount>
                {results.tickets.map((t) => (
                  <CommandItem key={t.id} value={`ticket ${t.id}`} forceMount onSelect={() => go(`/tickets/${t.id}`)}>
                    <TicketIcon aria-hidden />
                    <Mono className="shrink-0 text-current">{t.id}</Mono>
                    <span className="min-w-0 truncate">{t.title}</span>
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
            {results.triggers.length > 0 && (
              <CommandGroup heading="Triggers" forceMount>
                {results.triggers.map((t) => (
                  <CommandItem
                    key={t.id}
                    value={`trigger ${t.id}`}
                    forceMount
                    onSelect={() => go(`/routing?${new URLSearchParams({ tab: "triggers", trigger: t.id })}`)}
                  >
                    <ZapIcon aria-hidden />
                    <span className="min-w-0 truncate">{t.text}</span>
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
            {results.threads.length > 0 && (
              <CommandGroup heading="Threads" forceMount>
                {results.threads.map((t) => (
                  <CommandItem
                    key={t.key}
                    value={`thread ${t.key}`}
                    forceMount
                    onSelect={() => go(`/routing?${new URLSearchParams({ tab: "threads", thread: t.key })}`)}
                  >
                    <HashIcon aria-hidden />
                    <Mono className="min-w-0 truncate text-current">{t.key}</Mono>
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
            <CommandGroup heading="Go to">
              {NAV.filter((n) => n.to !== "/new").map((n) => (
                <CommandItem key={n.to} value={`go ${n.label}`} onSelect={() => go(n.to)}>
                  <n.icon aria-hidden />
                  {n.label}
                </CommandItem>
              ))}
            </CommandGroup>
            <CommandGroup heading="Actions">
              <CommandItem value="new task" onSelect={() => go("/new")}>
                <PlusIcon aria-hidden />
                New task
              </CommandItem>
              <CommandItem
                value="toggle theme"
                onSelect={() => {
                  setTheme(resolved === "dark" ? "light" : "dark");
                  setOpen(false);
                }}
              >
                <SunMoonIcon aria-hidden />
                {resolved === "dark" ? "Switch to the light theme" : "Switch to the dark theme"}
              </CommandItem>
            </CommandGroup>
          </>
        )}
      </CommandList>
    </CommandDialog>
  );
}
