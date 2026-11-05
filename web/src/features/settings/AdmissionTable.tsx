// The GitHub Source's admission table: for each receiving scope (row) and each kind of sender and message (column),
// what a message becomes. Each cell offers only the choices internal/policy accepts for it; a CI result has no sender,
// so its row has only the last cell. On the desktop it is a table; on the phone each row is a group of its cells.
import type { Layout } from "@/app/Panes";
import { Select } from "@/ui/primitives/select";
import { type Admission, CELLS, type Cell, cellValue, choicesFor, SCOPES, type Scope } from "./policy";

const ROW_LABEL: Record<Scope, string> = { issues: "Issues", pulls: "Pull requests", comments: "Comments", ci: "CI" };
const CELL_LABEL: Record<Cell, string> = {
  listedUnaddressed: "Listed unaddressed",
  unlistedAddressed: "Unlisted addressed",
  unlistedUnaddressed: "Unlisted unaddressed",
};
const CHOICE_LABEL: Record<string, string> = { explicit: "Explicit", inferred: "Inferred", ignore: "Ignore" };

type Props = { value: Admission; onChange(next: Admission): void; layout: Layout; disabled?: boolean };

function CellSelect({ scope, cell, value, onChange, disabled }: Props & { scope: Scope; cell: Cell }) {
  const choices = choicesFor(scope, cell);
  if (choices.length === 0) {
    return (
      <span className="text-muted">
        <span aria-hidden>—</span>
        <span className="sr-only">Not applicable</span>
      </span>
    );
  }
  return (
    <Select
      aria-label={`${ROW_LABEL[scope]}, ${CELL_LABEL[cell].toLowerCase()}`}
      value={cellValue(value[scope], cell)}
      disabled={disabled}
      onChange={(e) => onChange({ ...value, [scope]: { ...value[scope], [cell]: e.target.value } })}
    >
      {choices.map((c) => (
        <option key={c} value={c}>
          {CHOICE_LABEL[c] ?? c}
        </option>
      ))}
    </Select>
  );
}

export function AdmissionTable(props: Props) {
  if (props.layout === "phone") {
    return (
      <div className="grid gap-4">
        {SCOPES.map((scope) => (
          <fieldset key={scope} className="grid min-w-0 gap-2 border-t border-line-soft pt-3">
            <legend className="pb-1 font-semibold">{ROW_LABEL[scope]}</legend>
            {CELLS.filter((cell) => choicesFor(scope, cell).length > 0).map((cell) => (
              <label key={cell} className="grid min-w-0 gap-1 text-sm">
                <span className="text-muted">{CELL_LABEL[cell]}</span>
                <CellSelect {...props} scope={scope} cell={cell} />
              </label>
            ))}
          </fieldset>
        ))}
      </div>
    );
  }
  return (
    <table className="w-full table-fixed border-collapse text-sm">
      <thead>
        <tr className="border-b border-line text-left">
          <th scope="col" className="w-32 py-2 pr-3 font-mono text-xs font-normal text-muted uppercase">
            Scope
          </th>
          {CELLS.map((cell) => (
            <th key={cell} scope="col" className="py-2 pr-3 font-semibold">
              {CELL_LABEL[cell]}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {SCOPES.map((scope) => (
          <tr key={scope} className="border-b border-line-soft">
            <th scope="row" className="py-2 pr-3 text-left font-semibold">
              {ROW_LABEL[scope]}
            </th>
            {CELLS.map((cell) => (
              <td key={cell} className="py-2 pr-3">
                <CellSelect {...props} scope={scope} cell={cell} />
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  );
}
