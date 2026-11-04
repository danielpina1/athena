import { lazy, Suspense } from "react";

// /design is the Precision kitchen sink: shipped, unlinked, and a lazy chunk, so it costs the first screen nothing.
// The owner previews against the real built server and the screenshot baselines use it. Task 9's router replaces
// this pathname check.
const DesignPage = lazy(() => import("@/app/routes/design"));

export function App() {
  if (window.location.pathname === "/design") {
    return (
      <Suspense fallback={null}>
        <DesignPage />
      </Suspense>
    );
  }
  return (
    <main>
      <h1>Athena</h1>
    </main>
  );
}
