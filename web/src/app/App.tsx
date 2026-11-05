import { lazy, Suspense } from "react";
import { BrowserRouter, Route, Routes, useNavigate } from "react-router";
import { boot, type Runtime } from "@/app/boot";
import { SessionGate } from "@/app/SessionGate";
import { Shell } from "@/app/Shell";
import { Enroll } from "@/features/auth/Enroll";
import { Toaster } from "@/ui/primitives/toast";

// /design is the Precision kitchen sink: shipped, unlinked, and a lazy chunk, so it costs the first screen nothing.
// The owner previews against the real built server and the screenshot baselines use it. Neither it nor /enroll needs
// a session (ruling R35); every other path is the signed-in app behind the session gate.
const DesignPage = lazy(() => import("@/app/routes/design"));

function EnrollPage() {
  const navigate = useNavigate();
  return <Enroll onDone={() => navigate("/", { replace: true })} />;
}

function SignedIn({ runtime }: { runtime?: Runtime }) {
  const rt = runtime ?? boot();
  return (
    <SessionGate runtime={rt}>
      <Shell link={rt.client} />
    </SessionGate>
  );
}

export function App({ runtime }: { runtime?: Runtime }) {
  return (
    <BrowserRouter>
      <Routes>
        <Route
          path="/design"
          element={
            <Suspense fallback={null}>
              <DesignPage />
            </Suspense>
          }
        />
        {/* One Toaster each (ruling R34); the kitchen sink mounts its own. */}
        <Route
          path="/enroll"
          element={
            <>
              <EnrollPage />
              <Toaster />
            </>
          }
        />
        <Route
          path="*"
          element={
            <>
              <SignedIn runtime={runtime} />
              <Toaster />
            </>
          }
        />
      </Routes>
    </BrowserRouter>
  );
}
