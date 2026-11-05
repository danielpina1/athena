import { type ReactNode, useEffect } from "react";
import { useStore } from "zustand";
import type { Runtime } from "@/app/boot";
import { ReauthDialog } from "@/features/auth/ReauthDialog";
import { SignIn } from "@/features/auth/SignIn";

// Shows the app only to a signed-in owner: sign-in otherwise (with a notice when a session ended under the open app).
// Task 9's router goes inside, as the children. While the first answer is on its way the page is just the name.
export function SessionGate({ runtime, children }: { runtime: Runtime; children: ReactNode }) {
  const phase = useStore(runtime.gate, (s) => s.phase);
  useEffect(() => {
    void runtime.checkSession();
  }, [runtime]);

  switch (phase.kind) {
    case "checking":
      return (
        <main aria-busy="true" className="grid min-h-dvh place-items-center">
          <h1 className="text-2xl font-semibold">Athena</h1>
        </main>
      );
    case "signedOut":
      return <SignIn notice={phase.notice} onSignedIn={runtime.signedIn} />;
    case "signedIn":
      return (
        <>
          {children}
          <ReauthDialog />
        </>
      );
  }
}
