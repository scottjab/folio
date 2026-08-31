import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    // jsdom throughout, so the editor tests can mount a real EditorView. That
    // is the only way to catch the class of bug that shows up as a blank page
    // rather than a failed assertion: a module that throws while loading, or an
    // extension that fails to configure.
    environment: "jsdom",
    setupFiles: ["./src/test-setup.ts"],
  },
});
