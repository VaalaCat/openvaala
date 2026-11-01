import { execFile } from "node:child_process"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { promisify } from "node:util"

// 编译 internal/e2e/webuifixture 和 openvaala（嵌入 www/out/web 里刚构建的页面），把两个二进制的路径放进
// WEBUI_FIXTURE_BINARY、OPENVAALA_BINARY 交给各 worker 启动自己的 fixture；返回的 teardown 删掉编译目录
export default async function globalSetup(): Promise<() => Promise<void>> {
  const directory = await mkdtemp(
    path.join(tmpdir(), "openvaala-webui-fixture-")
  )
  const remove = () => rm(directory, { recursive: true, force: true })
  try {
    // 1. 并行编译两个二进制（直接运行二进制，SIGTERM 才能送到 fixture；go run 不转发信号）。
    const fixtureBinary = path.join(directory, "webuifixture")
    const openvaalaBinary = path.join(directory, "openvaala")
    const repository = path.join(import.meta.dirname, "..", "..")
    await Promise.all([
      promisify(execFile)(
        "go",
        ["build", "-o", fixtureBinary, "./internal/e2e/webuifixture"],
        { cwd: repository }
      ),
      promisify(execFile)(
        "go",
        ["build", "-o", openvaalaBinary, "./cmd/openvaala"],
        { cwd: repository }
      ),
    ])
    process.env.WEBUI_FIXTURE_BINARY = fixtureBinary
    process.env.OPENVAALA_BINARY = openvaalaBinary
  } catch (error) {
    await remove()
    throw error
  }
  // 2. teardown：删编译目录。
  return remove
}
