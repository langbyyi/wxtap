import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    coverage: {
      provider: "v8",
      // Only the Core runtime source is measured: hooks/*.js and scripts run
      // in other environments (page context / CLI) and are covered by their
      // own gates.
      include: ["src/**/*.ts"],
      exclude: ["src/**/*.test.ts"],
      reporter: ["text", "text-summary"],
      // Floors sit just under the current numbers so quality can only move
      // up; raise them when the suite improves.
      thresholds: {
        statements: 84,
        branches: 80,
        functions: 96,
        lines: 84,
      },
    },
  },
});