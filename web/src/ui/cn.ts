import { type ClassValue, clsx } from "clsx";
import { extendTailwindMerge } from "tailwind-merge";

// tailwind-merge must know the token font sizes, or it reads `text-body` as a colour and drops it next to
// `text-muted`.
const twMerge = extendTailwindMerge({
  extend: { theme: { text: ["body"], radius: ["xs", "md"] } },
});

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
