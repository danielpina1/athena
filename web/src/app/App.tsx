import { lazy, Suspense } from "react";

// /design is the Precision kitchen sink, for development only: production builds drop it. Task 9 replaces this
// check with the router.
const DesignPage = import.meta.env.DEV ? lazy(() => import("@/app/routes/design")) : null;

export function App() {
  if (DesignPage && window.location.pathname === "/design") {
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
