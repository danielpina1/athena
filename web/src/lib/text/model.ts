// A model id as people say it, shared by the session view and the timeline (it lives here, outside the session view's
// lazy chunk, so the timeline does not pull that chunk in).

/** A model id as people say it: claude-haiku-4-5-20251001 is "haiku 4.5"; an id of another shape stays as it is. */
export function modelName(id: string): string {
  let m = /^claude-([a-z]+)-(\d+)(?:-(\d{1,2}))?(?:-\d{8})?(?:\[.*\])?$/.exec(id);
  if (m) return `${m[1]} ${m[2]}${m[3] ? "." + m[3] : ""}`;
  m = /^claude-(\d+)(?:-(\d{1,2}))?-([a-z]+)(?:-\d{8})?$/.exec(id);
  if (m) return `${m[3]} ${m[1]}${m[2] ? "." + m[2] : ""}`;
  return id;
}
