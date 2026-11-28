import { writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";

const clientOutDir = fileURLToPath(new URL("../client/frontend/dist", import.meta.url));

// The Go client embeds client/frontend/dist, which is kept in git as an empty directory so the app
// still compiles before the UI has been built. Put the placeholder back after emptyOutDir clears it.
const keepClientDistPlaceholder = (): Plugin => ({
  name: "ourchive-keep-client-dist",
  closeBundle() {
    writeFileSync(resolve(clientOutDir, ".gitkeep"), "");
  },
});

// `vite build --mode client` bundles the UI into the desktop app (see src/platform.ts).
export default defineConfig(({ mode }) => ({
  plugins: mode === "client" ? [react(), keepClientDistPlaceholder()] : [react()],
  build: mode === "client" ? { outDir: clientOutDir, emptyOutDir: true } : undefined,
  optimizeDeps: {
    exclude: ["input-switch-polyfill"],
  },
  server: {
    port: 5173,
    // In the desktop app's dev mode, /api never reaches Vite: the app's Go side answers it first.
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
      },
    },
  },
}));
