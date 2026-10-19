import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["dist", "playwright-report", "test-results", "node_modules"] },
  {
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    files: ["**/*.{ts,tsx}"],
    languageOptions: { ecmaVersion: 2022, globals: { ...globals.browser, ...globals.node } },
    plugins: { "react-hooks": reactHooks, "react-refresh": reactRefresh },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": ["warn", { allowConstantExport: true }],
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_", varsIgnorePattern: "^_" }],
    },
  },
  {
    // Context/provider modules export hooks next to their provider by design;
    // the rule only concerns dev-time hot reloading.
    files: ["src/components/ui/**/*.tsx", "src/lib/**/*.tsx", "src/app/**/*.tsx", "src/components/shell/**/*.tsx", "src/components/common/**/*.tsx", "src/App.tsx", "src/test/**", "e2e/**"],
    rules: { "react-refresh/only-export-components": "off" },
  },
);
