// The command palette (⌘K, or the search button on the phone): go to any route; find a Ticket by id or title, a
// Trigger by its text, a Thread by its key ("#12", or a GitHub URL); act on the open Ticket (Reply, Stop, Retry,
// Interrupt, Watch session); start a new task; switch the theme. Reply asks for its text and Stop for a confirmation
// in a second step of the palette itself.
import {
  CornerDownLeftIcon,
  EyeIcon,
  HashIcon,
  type LucideIcon,
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
import type { TicketSummary } from "@/gen/athena/v1/state_pb";
import { NAV } from "@/app/routes";
import { search, type SearchHit } from "@/app/search";
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

type Results = {
  tickets: Extract<SearchHit, { kind: "ticket" }>[];
  triggers: Extract<SearchHit, { kind: "trigger" }>[];
  threads: Extract<SearchHit, { kind: "thread" }>[];
};

function useResults(q: string): Results {
  const tickets = useAthena((s) => s.tickets);
  const triggers = useAthena((s) => s.triggers);
  const threads = useAthena((s) => s.threads);
  return useMemo(() => {
    const hits = search({ tickets, triggers, threads }, q);
    return {
      tickets: hits.filter((h) => h.kind === "ticket"),
      triggers: hits.filter((h) => h.kind === "trigger"),
      threads: hits.filter((h) => h.kind === "thread"),
    };
  }, [q, tickets, triggers, threads]);
}

type Step = { kind: "root" } | { kind: "reply"; id: string } | { kind: "stop"; id: string };
const ROOT: Step = { kind: "root" };

type PaletteAction = { key: string; label: string; icon: LucideIcon; run: () => void };

/** The actions whose label or key holds every word of q, ignoring case; all of them for an empty q. */
function keep(q: string, list: PaletteAction[]): PaletteAction[] {
  const words = q.trim().toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return list;
  return list.filter((a) => {
    const text = `${a.label} ${a.key}`.toLowerCase();
    return words.every((w) => text.includes(w));
  });
}

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
  const [text, setText] = useState("");
  const results = useResults(step.kind === "root" ? text : "");

  const setOpen = (next: boolean) => {
    if (!next) {
      setStep(ROOT);
      setText("");
    }
    onOpenChange(next);
  };
  const go = (to: string) => {
    setOpen(false);
    navigate(to);
  };
  const toStep = (next: Step) => {
    setStep(next);
    setText("");
  };
  // Backspace in an empty field leaves a step.
  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (step.kind !== "root" && e.key === "Backspace" && text === "") {
      e.preventDefault();
      toStep(ROOT);
    }
  };

  /** What can be done to the open Ticket: Reply and Stop take a second step; Retry and Interrupt when they apply. */
  const ticketActionsFor = (t: TicketSummary): PaletteAction[] => {
    const now = (done: string, failed: string, send: (id: string) => Promise<CommandResult>) => () => {
      void report(done, failed, send(t.id));
      setOpen(false);
    };
    const list: PaletteAction[] = [
      { key: `reply ${t.id}`, label: `Reply to ${t.id}`, icon: CornerDownLeftIcon, run: () => toStep({ kind: "reply", id: t.id }) },
    ];
    if (t.stage !== "ended") {
      list.push({ key: `stop ${t.id}`, label: `Stop ${t.id}`, icon: OctagonXIcon, run: () => toStep({ kind: "stop", id: t.id }) });
    }
    if (t.canRetry) {
      list.push({ key: `retry ${t.id}`, label: `Retry ${t.id}`, icon: RotateCcwIcon, run: now("Retrying", "Not retried", retry) });
    }
    if (t.stage === "working") {
      list.push({
        key: `interrupt ${t.id}`,
        label: `Interrupt ${t.id}`,
        icon: PauseIcon,
        run: now("Interrupted", "Not interrupted", interrupt),
      });
    }
    list.push({ key: `watch session ${t.id}`, label: "Watch session", icon: EyeIcon, run: () => go(`/tickets/${t.id}?tab=session`) });
    return list;
  };

  // Everything is filtered here, not by cmdk: its fuzzy filter would hide (and not count) a result found another way,
  // a GitHub URL for a Thread key say, and show "Nothing matches." beside it.
  const q = step.kind === "root" ? text : "";
  const ticketActions = keep(q, ticket ? ticketActionsFor(ticket) : []);
  const goTo = keep(
    q,
    NAV.filter((n) => n.to !== "/new").map((n) => ({
      key: `go ${n.label}`,
      label: n.label,
      icon: n.icon,
      run: () => go(n.to),
    })),
  );
  const actions = keep(q, [
    { key: "new task", label: "New task", icon: PlusIcon, run: () => go("/new") },
    {
      key: "toggle theme",
      label: resolved === "dark" ? "Switch to the light theme" : "Switch to the dark theme",
      icon: SunMoonIcon,
      run: () => {
        setTheme(resolved === "dark" ? "light" : "dark");
        setOpen(false);
      },
    },
  ]);
  const visible = [...ticketActions, ...results.tickets, ...results.triggers, ...results.threads, ...goTo, ...actions];

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
      commandProps={{ shouldFilter: false, loop: true, label: "Command palette" }}
    >
      <CommandInput value={text} onValueChange={setText} placeholder={placeholder} onKeyDown={onKeyDown} />
      <CommandList>
        {step.kind === "reply" && (
          <CommandGroup heading={`Reply to ${step.id}`}>
            <CommandItem
              value="send"
              disabled={text.trim() === ""}
              onSelect={() => {
                void report("Reply sent", "Reply not sent", reply(step.id, text.trim()));
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
            {visible.length === 0 && <CommandEmpty>Nothing matches.</CommandEmpty>}
            {ticket && (
              <CommandGroup heading={ticket.id}>
                {ticketActions.map((a) => (
                  <CommandItem key={a.key} value={a.key} onSelect={a.run}>
                    <a.icon aria-hidden />
                    {a.label}
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
            {results.tickets.length > 0 && (
              <CommandGroup heading="Tickets">
                {results.tickets.map((t) => (
                  <CommandItem key={t.id} value={`ticket ${t.id}`} onSelect={() => go(t.to)}>
                    <TicketIcon aria-hidden />
                    <Mono className="shrink-0 text-current">{t.id}</Mono>
                    <span className="min-w-0 truncate">{t.title}</span>
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
            {results.triggers.length > 0 && (
              <CommandGroup heading="Triggers">
                {results.triggers.map((t) => (
                  <CommandItem
                    key={t.id}
                    value={`trigger ${t.id}`}
                    onSelect={() => go(t.to)}
                  >
                    <ZapIcon aria-hidden />
                    <span className="min-w-0 truncate">{t.text}</span>
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
            {results.threads.length > 0 && (
              <CommandGroup heading="Threads">
                {results.threads.map((t) => (
                  <CommandItem
                    key={t.key}
                    value={`thread ${t.key}`}
                    onSelect={() => go(t.to)}
                  >
                    <HashIcon aria-hidden />
                    <Mono className="min-w-0 truncate text-current">{t.key}</Mono>
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
            {goTo.length > 0 && (
              <CommandGroup heading="Go to">
                {goTo.map((a) => (
                  <CommandItem key={a.key} value={a.key} onSelect={a.run}>
                    <a.icon aria-hidden />
                    {a.label}
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
            {actions.length > 0 && (
              <CommandGroup heading="Actions">
                {actions.map((a) => (
                  <CommandItem key={a.key} value={a.key} onSelect={a.run}>
                    <a.icon aria-hidden />
                    {a.label}
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
          </>
        )}
      </CommandList>
    </CommandDialog>
  );
}
