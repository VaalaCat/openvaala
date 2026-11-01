import { defineConfig, devices } from "@playwright/test"

// 端到端测试连真实的 serve：globalSetup 编译 internal/e2e/webuifixture，每个 worker 起一份自己的 serve 和假模型，
// 用例分散到各 worker 并行跑。connect.spec.ts 要看空的对话列表，放在单独的 project 里，拿到自己的 worker 和 serve
export default defineConfig({
  testDir: "e2e",
  globalSetup: "./e2e/global-setup.ts",
  workers: 4,
  fullyParallel: true,
  retries: 0,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: "list",
  use: {
    ...devices["Desktop Chrome"],
    locale: "zh-CN",
    trace: "retain-on-failure",
  },
  projects: [
    { name: "connect", testMatch: "connect.spec.ts" },
    { name: "app", testIgnore: "connect.spec.ts" },
  ],
})
