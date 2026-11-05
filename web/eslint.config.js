import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["node_modules", "src/gen", "../internal/web/ui/dist", "test-results", "playwright-report"] },
  ...tseslint.configs.recommended,
  // React's rules are for the app; Playwright's fixtures call a `use` that is not React's.
  { ...reactHooks.configs.flat.recommended, files: ["src/**/*.{ts,tsx}"] },
);
