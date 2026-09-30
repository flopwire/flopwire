import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    host: "127.0.0.1",
    port: 5173,
    proxy: { "/v1": "http://127.0.0.1:58080", "/healthz": "http://127.0.0.1:58080" },
  },
  test: { environment: "jsdom", globals: true },
});
