// The New screen's model: a native select of the chosen runtime's models, as the node's last check of it listed them
// (Claude Code's aliases; Codex's model/list). "Default · <name>" sends no model, so the runtime's own default runs;
// "Other…" opens a free-text field, with the runtime's recent models as suggestions, for a model the list lacks. A
// runtime whose models are not known yet (not checked, or not ready) offers Default and Other… only.
import { useId } from "react";
import type { RuntimeModel, RuntimeStatus } from "@/gen/athena/v1/state_pb";
import { Input } from "@/ui/primitives/input";
import { Select } from "@/ui/primitives/select";

/** The select's value for Other…: no model id is empty, and none starts with a control character. */
export const OTHER = "\u0001other";

/** The models a task on `runtime` may pick, Claude Code's under Auto (the only runtime a model applies on there). */
export function modelsFor(runtime: string, runtimes: readonly RuntimeStatus[]): readonly RuntimeModel[] {
  const name = runtime === "auto" ? "claude" : runtime;
  return runtimes.find((r) => r.name === name)?.models ?? [];
}

/** Whether `choice` (a select value) is offered for `models`: Default and Other… always are. */
export function offered(choice: string, models: readonly RuntimeModel[]): boolean {
  return choice === "" || choice === OTHER || models.some((m) => m.id === choice);
}

export function ModelPicker({
  value,
  onChange,
  other,
  onOtherChange,
  models,
  suggestions,
  hint,
}: {
  /** "" for Default, OTHER, or a model's id. */
  value: string;
  onChange(value: string): void;
  other: string;
  onOtherChange(text: string): void;
  models: readonly RuntimeModel[];
  suggestions: readonly string[];
  /** A line under the field that the select names with aria-describedby. */
  hint?: string;
}) {
  const list = useId();
  const hintId = useId();
  const fallback = models.find((m) => m.default);
  return (
    <div className="grid min-w-0 gap-1">
      <label className="grid gap-1 text-sm font-semibold">
        Model
        <Select
          value={value}
          onChange={(e) => onChange(e.target.value)}
          aria-describedby={hint ? hintId : undefined}
          className="font-normal"
        >
          <option value="">{fallback ? `Default · ${fallback.name}` : "Default"}</option>
          {models.map((m) => (
            <option key={m.id} value={m.id}>
              {m.name}
            </option>
          ))}
          <option value={OTHER}>Other…</option>
        </Select>
      </label>
      {value === OTHER && (
        <>
          <Input
            role="combobox"
            aria-label="Other model"
            aria-expanded={false}
            aria-autocomplete="list"
            list={list}
            value={other}
            onChange={(e) => onOtherChange(e.target.value)}
            placeholder="Model id"
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            className="font-mono text-sm"
          />
          <datalist id={list}>
            {suggestions.map((m) => (
              <option key={m} value={m} />
            ))}
          </datalist>
        </>
      )}
      {hint && (
        <p id={hintId} className="text-sm text-muted">
          {hint}
        </p>
      )}
    </div>
  );
}
