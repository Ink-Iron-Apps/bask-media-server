import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

// The tests run inside workerd against a real D1, because the two things most
// worth testing here are the SQL and the crypto, and a mock of either would be
// a test of the mock.
export default defineConfig({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.jsonc" },
      miniflare: { d1Databases: ["DB"] },
    }),
  ],
});
