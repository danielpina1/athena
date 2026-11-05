// Settings › System: the screens that are not in the phone's navigation (Routing, Nodes), the server's version (from
// the link's Welcome) and the theme.
import { Link } from "react-router";
import { useAthena } from "@/lib/store/state";
import { Mono } from "@/ui/athena/Mono";
import { Waiting } from "@/ui/athena/Waiting";
import { ToggleGroup, ToggleGroupItem } from "@/ui/primitives/toggle-group";
import { type ThemeChoice, useTheme } from "@/ui/theme";
import { Block, Section } from "./Section";

const linkClass =
  "inline-flex min-h-11 items-center font-semibold underline decoration-line-soft underline-offset-4 hover:decoration-text lg:min-h-0";

export function System() {
  const version = useAthena((s) => s.serverVersion);
  const { theme, setTheme } = useTheme();
  return (
    <Section title="System" text="Athena's other screens, its version and how it looks.">
      <Block title="Screens">
        <ul className="grid gap-1">
          <li>
            <Link to="/routing" className={linkClass}>
              Routing
            </Link>
            <span className="ml-2 text-sm text-muted">How incoming messages were routed to Tickets.</span>
          </li>
          <li>
            <Link to="/nodes" className={linkClass}>
              Nodes
            </Link>
            <span className="ml-2 text-sm text-muted">The machines that run sessions, and their health.</span>
          </li>
        </ul>
      </Block>
      <Block title="Server version">{version ? <Mono tone="default">{version}</Mono> : <Waiting />}</Block>
      <Block title="Appearance">
        <ToggleGroup
          type="single"
          aria-label="Appearance"
          value={theme}
          onValueChange={(v) => v && setTheme(v as ThemeChoice)}
          className="justify-self-start"
        >
          <ToggleGroupItem value="system">System</ToggleGroupItem>
          <ToggleGroupItem value="light">Light</ToggleGroupItem>
          <ToggleGroupItem value="dark">Dark</ToggleGroupItem>
        </ToggleGroup>
      </Block>
    </Section>
  );
}
