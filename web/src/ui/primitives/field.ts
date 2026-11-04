// The shared look of text fields: a 1 px --line hairline, the token radius, the accent focus outline from the base
// layer, and --danger for aria-invalid.
export const fieldClass = [
  "w-full min-w-0 rounded-md border border-line bg-surface text-body text-text",
  "placeholder:text-muted disabled:cursor-not-allowed disabled:opacity-40",
  "aria-invalid:border-danger aria-invalid:outline-danger",
].join(" ");
