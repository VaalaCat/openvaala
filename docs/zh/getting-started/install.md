# 安装

这一页介绍安装 OpenVaala 的几种方式，以及每种方式包含哪些部分。

OpenVaala 支持 Linux 和 macOS，amd64 和 arm64 都可以，不支持 Windows。有三种安装方式：

| | 终端界面、Runtime 服务、编辑器、Codex | Web 界面 | 桌面端 |
| --- | --- | --- | --- |
| [GitHub Releases](#从-github-releases-安装) | 有 | 有 | 单独的安装包 |
| [`go install`](#用-go-install-安装) | 有 | 有 | 命令行程序，不是安装包 |
| [从源码构建](#从源码构建) | 有 | 有 | 有 |

## 从 GitHub Releases 安装

每个 [Release](https://github.com/VaalaCat/openvaala/releases) 里有：

- **各平台的 `openvaala` 二进制**，已经内置 Web 界面。下载适合你系统的那个，加上可执行权限，放进 `PATH`：

  ```bash
  chmod +x openvaala-linux-amd64
  sudo mv openvaala-linux-amd64 /usr/local/bin/openvaala
  openvaala --version
  ```

- **桌面端安装包**，有 macOS 版（同时支持 Apple 芯片和 Intel 的通用版，需要 macOS 12 或更高）和 Linux 版。桌面端里也带着完整的命令行工具；怎么在终端里用它、Linux 需要装什么，见[桌面端](../guides/desktop.md)。

## 用 go install 安装

装了 Go 的话，下面的命令把命令行工具装到 `$(go env GOPATH)/bin`：

```bash
go install github.com/VaalaCat/openvaala/cmd/openvaala@latest
```

同一条命令换成 `cmd/openvaala-desktop`，会另外装一个名叫 `openvaala-desktop` 的程序，也放在 `$(go env GOPATH)/bin`。直接运行它会打开桌面窗口，里面同样有完整的命令行工具。这不是桌面端安装包：没有 `.app`、磁盘映像，也不会出现在应用菜单里。Linux 上仍然需要 GTK 3，WebView 还需要 WebKitGTK 4.1。见[桌面端](../guides/desktop.md)。

```bash
go install github.com/VaalaCat/openvaala/cmd/openvaala-desktop@latest
openvaala-desktop                 # WebView 窗口
openvaala-desktop app --native    # 原生界面
```

发布版本的 tag 包含已经构建好的 Web 页面，所以用 `@latest` 或指定发布版本安装上面两个程序后，都可以直接使用 Web 界面，不需要 Node.js 或 pnpm。开发分支不把生成的页面放进 Git，因此 `@main` 不带 Web 界面，这个桌面端程序也打不开 WebView；要使用开发版的 Web 界面，请按下面的说明从源码构建。在本仓库的检出目录里，用 `go install ./cmd/openvaala` 或 `go install ./cmd/openvaala-desktop` 安装你当前检出的版本。

需要 Go 1.27.3 或更高版本。

## 从源码构建

从源码构建能得到带 Web 界面的 CLI，也可以顺便构建桌面端。

需要：

- Go 1.27.3 或更高版本，以及 Git
- Node.js 24 或更高版本，以及 pnpm，用来构建 Web 界面

构建不需要 C 工具链。在 Linux 上运行桌面端需要 GTK 3；WebView 界面还需要 WebKitGTK 4.1。

```bash
git clone https://github.com/VaalaCat/openvaala.git
cd openvaala
script/build.sh
```

`script/build.sh` 先构建 Web 界面并打包进二进制，然后为本机平台生成 `dist/openvaala-<os>-<arch>`。常用选项：

- `--matrix`：为 Linux 和 macOS 的 amd64、arm64 都构建一份。
- `--os linux --arch amd64,arm64`：指定目标平台。
- `--skip-frontend`：不构建 Web 界面，沿用已经构建好的（没有就不带 Web 界面）。
- `--version v0.2.0`：指定版本号；默认用最近的 Git tag。
- `--output DIR`：输出到 `dist/` 以外的目录。

构建桌面端：

```bash
script/build-desktop.sh                          # macOS 通用版，Linux amd64 和 arm64
script/build-desktop.sh --platform linux/amd64   # 只构建一个目标
```

产物在 `dist/desktop/`。生成 macOS 的 `.dmg`、代码签名和公证需要一台 macOS 机器和对应的凭据；在其他机器上，脚本照样生成 `.app`，跳过这几步（也可以用 `--skip-dmg` 和 `--skip-notarize` 明确跳过）。

两个脚本都可以加 `--help` 查看全部选项。

## 下一步

接着看[快速开始](quickstart.md)，添加模型，开始第一个对话。
