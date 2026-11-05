import { NavLink, Outlet } from "react-router-dom";
import type { ReactNode } from "react";

const icon = (d: ReactNode) => (
  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    {d}
  </svg>
);

const NAV = [
  {
    to: "/",
    label: "Dashboard",
    icon: icon(
      <>
        <rect width="7" height="9" x="3" y="3" rx="1" />
        <rect width="7" height="5" x="14" y="3" rx="1" />
        <rect width="7" height="9" x="14" y="12" rx="1" />
        <rect width="7" height="5" x="3" y="16" rx="1" />
      </>,
    ),
  },
  { to: "/tasks", label: "Tasks", icon: icon(<><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></>) },
  { to: "/executions", label: "Executions", icon: icon(<path d="M22 12h-4l-3 9L9 3l-3 9H2" />) },
  {
    to: "/integrations",
    label: "Integrations",
    icon: icon(<><path d="M12 22v-5" /><path d="M9 8V2" /><path d="M15 8V2" /><path d="M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z" /></>),
  },
  {
    to: "/settings",
    label: "Settings",
    icon: icon(<><path d="M21 4h-7M10 4H3M21 12h-9M8 12H3M21 20h-5M12 20H3M14 2v4M8 10v4M16 18v4" /></>),
  },
];

export function Layout({ version }: { version?: string }) {
  return (
    <div className="flex h-full">
      <aside className="flex w-60 shrink-0 flex-col gap-1 border-r border-line bg-surface px-4 py-5">
        <div className="flex items-center gap-2.5 px-2 pb-5">
          <svg width="22" height="22" viewBox="0 0 24 24" fill="none" aria-hidden>
            <circle cx="12" cy="12" r="9" stroke="var(--color-brass)" strokeWidth="2" />
            <path d="M12 7v5l3 2" stroke="var(--color-brass)" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
          </svg>
          <span className="text-[15px] font-semibold">Clockwork</span>
          {version && <span className="font-mono text-[11px] text-fg-3">{version}</span>}
        </div>
        <nav className="flex flex-col gap-1" aria-label="Main">
          {NAV.map((n) => (
            <NavLink
              key={n.to}
              to={n.to}
              end={n.to === "/"}
              className={({ isActive }) =>
                `flex h-[34px] items-center gap-2.5 rounded-md px-2.5 text-[13px] transition ${
                  isActive ? "bg-raised font-medium text-fg [&_svg]:text-brass" : "text-fg-2 hover:bg-raised/60 hover:text-fg"
                }`
              }
            >
              {n.icon}
              {n.label}
            </NavLink>
          ))}
        </nav>
      </aside>
      <main className="min-w-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-8 py-7">
          <Outlet />
        </div>
      </main>
    </div>
  );
}
