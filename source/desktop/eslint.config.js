import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import tseslint from "typescript-eslint";

/**
 * Flat config for the desktop webview. `bulwark scan` runs it, so a finding
 * here fails the security gate rather than only a local run.
 *
 * `dist/` is build output and `src-tauri/` is Rust — cargo fmt and clippy own
 * that half.
 */
export default tseslint.config(
  { ignores: ["dist/", "src-tauri/"] },
  js.configs.recommended,
  // The type-checked set rather than the plain one: the popover drives the
  // daemon entirely through promises, so a dropped `invoke` is the mistake
  // worth catching, and only type information sees it.
  tseslint.configs.recommendedTypeChecked,
  reactHooks.configs.flat.recommended,
  {
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
  },
  {
    // The build configuration belongs to no TypeScript project that resolves
    // node's types, so every value in it is `any` to the type-checked rules and
    // they report the config itself rather than the code.
    files: ["*.config.ts", "eslint.config.js"],
    extends: [tseslint.configs.disableTypeChecked],
  },
  {
    files: ["src/**/*.test.{ts,tsx}"],
    rules: {
      // `await act(async () => …)` is what flushes React's effect queue: the
      // async form selects the flushing path, and the callback has nothing of
      // its own to await.
      "@typescript-eslint/require-await": "off",
    },
  },
);
