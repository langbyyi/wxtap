import js from "@eslint/js";
import tseslint from "typescript-eslint";
import globals from "globals";

export default tseslint.config(
  { ignores: ["dist/", "node_modules/", "coverage/"] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  {
    rules: {
      // The RPC layer legitimately widens unknown JSON payloads; narrowing
      // happens at each call site instead of casting at the boundary.
      "@typescript-eslint/no-explicit-any": "off",
      // Page-side hooks wrap guarded calls in empty catch blocks by design.
      "no-empty": ["error", { allowEmptyCatch: true }],
    },
  },
  {
    // Type-aware rules only where a tsconfig covers the file: the Core
    // runtime source. Scripts, hooks and this config are plain JS.
    files: ["src/**/*.ts"],
    extends: [...tseslint.configs.recommendedTypeChecked],
    languageOptions: {
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
    rules: {
      // Async stubs and Node callbacks without awaits are idiomatic here;
      // the rule flags intent, not defects.
      "@typescript-eslint/require-await": "off",
      // JSON-RPC payloads are unknown by design and narrowed at call sites.
      "@typescript-eslint/no-unsafe-assignment": "off",
      "@typescript-eslint/no-unsafe-member-access": "off",
      "@typescript-eslint/no-unsafe-argument": "off",
      "@typescript-eslint/no-unsafe-return": "off",
    },
  },
  {
    // Core runtime and tooling: Node globals.
    files: ["src/**/*.ts", "scripts/**/*.mjs"],
    languageOptions: { globals: globals.node },
  },
  {
    // Page-side injection hooks run inside the mini-program JS context.
    files: ["hooks/**/*.js"],
    languageOptions: { globals: { ...globals.browser, wx: "readonly" } },
  },
  {
    // Injection hooks are hand-written ES5 loaded verbatim into mini-program
    // pages: `const self = this` aliasing and guarded empty catches are the
    // established pattern there; rewriting them is not worth the risk.
    files: ["hooks/**/*.js"],
    rules: {
      "@typescript-eslint/no-this-alias": "off",
      "@typescript-eslint/no-unused-vars": ["error", { caughtErrors: "none" }],
      "no-redeclare": "off",
      "no-prototype-builtins": "off",
    },
  },
  {
    files: ["**/*.test.ts"],
    languageOptions: { globals: globals.node },
  },
);