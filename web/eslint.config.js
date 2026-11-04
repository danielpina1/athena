import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["node_modules", "src/gen", "../internal/web/ui/dist"] },
  ...tseslint.configs.recommended,
  reactHooks.configs.flat.recommended,
);
