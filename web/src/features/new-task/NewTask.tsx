// The New screen: describe the work, optionally pick a repository and a model, and Athena opens a Ticket. The text is
// required. ⌘⏎ (Ctrl+Enter) starts it from any field; Enter in the text adds a line. The fields keep what was typed
// until the result is ok, and a failure is said inline. Runtime and node pickers wait for a second choice (plans 5, 6).
import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { useNavigate } from "react-router";
import { modKeyLabel } from "@/app/shortcuts";
import { errorMessage, submitTask } from "@/lib/commands/actions";
import { selectTickets } from "@/lib/store/selectors";
import { useAthena } from "@/lib/store/state";
import { Button } from "@/ui/primitives/button";
import { Input } from "@/ui/primitives/input";
import { Textarea } from "@/ui/primitives/textarea";
import { RepoPicker, resolveRepo } from "./RepoPicker";

/** How many of the latest Tickets the model suggestions come from. */
const RECENT = 20;

export function NewTask() {
  const navigate = useNavigate();
  const [text, setText] = useState("");
  const [repoText, setRepoText] = useState("");
  const [model, setModel] = useState("");
  const [error, setError] = useState("");
  const [sending, setSending] = useState(false);
  const busy = useRef(false);
  const field = useRef<HTMLTextAreaElement>(null);
  const models = useId();
  const repos = useAthena((s) => s.repos);
  const tickets = useAthena((s) => selectTickets(s, {}));
  const suggestions = useMemo(
    () => [...new Set(tickets.slice(0, RECENT).map((t) => t.model).filter((m) => m !== ""))],
    [tickets],
  );
  const choice = resolveRepo(repoText, repos);
  const ready = text.trim() !== "" && choice.kind !== "invalid";

  // Autofocus on the desktop only: on the phone it would raise the keyboard over the form.
  useEffect(() => {
    if (window.matchMedia?.("(min-width: 1024px)").matches) field.current?.focus();
  }, []);

  const start = async () => {
    const t = text.trim();
    if (!ready || busy.current) return;
    busy.current = true;
    setSending(true);
    setError("");
    const arg: { text: string; repo?: string; dir?: string; model?: string } = { text: t };
    if (choice.kind === "repo") arg.repo = choice.repo;
    if (choice.kind === "dir") arg.dir = choice.dir;
    if (model.trim() !== "") arg.model = model.trim();
    const r = await submitTask(arg).finally(() => {
      busy.current = false;
      setSending(false);
    });
    if (!r.ok) {
      setError(errorMessage(r.code));
      return;
    }
    navigate(r.result.case === "ticketId" ? `/tickets/${r.result.value}` : "/tickets");
  };

  // Capture, so no field (the repository list's own Enter, the page's ⌘⏎ shortcut) acts on it first.
  const onKeyDownCapture = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key !== "Enter" || !(e.metaKey || e.ctrlKey)) return;
    // Safari ends the composition before the Enter that commits it: keyCode 229 is the only sign then.
    if (e.nativeEvent.isComposing || e.keyCode === 229) return;
    e.preventDefault();
    e.stopPropagation();
    if (!e.repeat) void start();
  };

  return (
    <section onKeyDownCapture={onKeyDownCapture} className="grid min-w-0 content-start gap-4 p-4 lg:max-w-2xl lg:p-6">
      <div className="grid gap-2">
        <h1 className="text-xl font-semibold">New task</h1>
        <p className="max-w-prose text-muted">Describe the work and Athena opens a Ticket for it.</p>
      </div>
      <label className="grid gap-1 text-sm font-semibold">
        What should Athena do?
        <Textarea
          ref={field}
          rows={5}
          value={text}
          onChange={(e) => setText(e.target.value)}
          className="text-[15px] font-normal lg:text-sm"
          placeholder={`Enter adds a line, ${modKeyLabel("⏎")} starts it`}
        />
      </label>
      <div className="grid gap-1 text-sm font-semibold">
        <span>Repository</span>
        <RepoPicker value={repoText} onChange={setRepoText} repos={repos} />
      </div>
      <label className="grid gap-1 text-sm font-semibold">
        Model
        <Input
          role="combobox"
          aria-expanded={false}
          aria-autocomplete="list"
          list={models}
          value={model}
          onChange={(e) => setModel(e.target.value)}
          placeholder="Default"
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          className="font-mono text-sm font-normal"
        />
        <datalist id={models}>
          {suggestions.map((m) => (
            <option key={m} value={m} />
          ))}
        </datalist>
      </label>
      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
      <div className="flex items-center justify-end gap-3">
        <span className="hidden text-sm text-muted lg:inline">{modKeyLabel("⏎")}</span>
        <Button variant="primary" onClick={() => void start()} disabled={!ready || sending}>
          Start
        </Button>
      </div>
    </section>
  );
}
