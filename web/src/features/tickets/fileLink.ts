// Where a link to a Ticket's file goes: the Ticket's page with ?file=<path>, which opens the viewer over it. On that
// page already, the other parameters (the open tab) stay.
import type { To } from "react-router";

export function fileLinkTo(here: { pathname: string; search: string }, file: string, ticketPath = here.pathname): To {
  const params = new URLSearchParams(ticketPath === here.pathname ? here.search : "");
  params.set("file", file);
  return { pathname: ticketPath, search: `?${params}` };
}

/** The Ticket's page with its Files tab open and no file. */
export function allFilesTo(here: { pathname: string; search: string }): To {
  const params = new URLSearchParams(here.search);
  params.delete("file");
  params.set("tab", "files");
  return { pathname: here.pathname, search: `?${params}` };
}
