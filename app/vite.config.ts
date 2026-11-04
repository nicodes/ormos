import { defineConfig } from "vite";
import solid from "vite-plugin-solid";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [solid(), tailwindcss()],
  build: {
    // Generated staging is embedded by crates/api/build.rs when packaging the tool.
    outDir: "../.artifacts/ui",
    emptyOutDir: true,
  },
});
