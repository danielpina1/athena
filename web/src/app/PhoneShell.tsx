// The phone shell (under 1024 px): a top bar with the name, the palette and the connection dot; the view; and the
// bottom navigation (Needs you with its count, Tickets, New, Graph, Settings). Every target is at least 44 px and the
// bars keep clear of the notch and the home indicator (safe-area insets).
import { SearchIcon } from "lucide-react";
import type { ReactNode } from "react";
import { NavLink } from "react-router";
import { ConnectionBanner, ConnectionDot, type LinkSource } from "@/app/ConnectionBanner";
import { UpdateBanner } from "@/app/UpdateBanner";
import { NAV, type NavItem, useNavCount } from "@/app/routes";
import { NeedsCount } from "@/app/Sidebar";
import { AthenaMark } from "@/ui/athena/AthenaMark";
import { cn } from "@/ui/cn";

function BottomLink({ item }: { item: NavItem }) {
  const count = useNavCount(item);
  const Icon = item.icon;
  const isNew = item.to === "/new";
  return (
    <li className="flex">
      <NavLink
        to={item.to}
        end={item.end}
        className={cn(
          "group relative flex min-h-14 flex-1 flex-col items-center justify-center gap-0.5 text-xs text-muted",
          "aria-[current=page]:font-semibold aria-[current=page]:text-text",
        )}
      >
        <span
          aria-hidden
          className={cn(
            "flex items-center justify-center",
            isNew ? "size-8 rounded-md bg-text text-bg" : "size-6",
          )}
        >
          <Icon className="size-5" />
        </span>
        <span className={cn(isNew && "sr-only")}>{item.label}</span>
        <NeedsCount n={count} className="absolute top-1 left-[calc(50%+6px)]" />
      </NavLink>
    </li>
  );
}

export function PhoneShell({
  link,
  onOpenPalette,
  children,
}: {
  link: LinkSource;
  onOpenPalette: () => void;
  children: ReactNode;
}) {
  return (
    <div className="flex min-h-dvh flex-col">
      <header className="sticky top-0 z-10 border-b border-line bg-bg pt-[env(safe-area-inset-top)]">
        <div className="flex h-12 items-center justify-between pr-[max(4px,env(safe-area-inset-right))] pl-[max(16px,env(safe-area-inset-left))]">
          <span className="flex items-center gap-2 font-mono text-xs tracking-wider text-muted uppercase">
            <AthenaMark />
            Athena
            <ConnectionDot link={link} />
          </span>
          <button
            type="button"
            onClick={onOpenPalette}
            aria-label="Search and commands"
            className="inline-flex size-11 items-center justify-center rounded-md text-text hover:bg-surface-2"
          >
            <SearchIcon className="size-5" aria-hidden />
          </button>
        </div>
        <ConnectionBanner link={link} />
        <UpdateBanner />
      </header>
      <main className="min-w-0 flex-1 pb-[calc(57px+env(safe-area-inset-bottom))]">{children}</main>
      <nav
        aria-label="Main"
        className="fixed inset-x-0 bottom-0 z-10 border-t border-line bg-bg pr-[env(safe-area-inset-right)] pb-[env(safe-area-inset-bottom)] pl-[env(safe-area-inset-left)]"
      >
        <ul className="grid grid-cols-5">
          {NAV.filter((n) => n.phone).map((item) => (
            <BottomLink key={item.to} item={item} />
          ))}
        </ul>
      </nav>
    </div>
  );
}
