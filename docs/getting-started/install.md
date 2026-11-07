# Install

This page shows the ways to install OpenVaala and what each one includes.

OpenVaala runs on Linux and macOS, on amd64 and arm64. Windows is not supported. There are three ways to get it:

| | Terminal UI, server, editors, Codex | Web UI | Desktop app |
| --- | --- | --- | --- |
| [GitHub Releases](#from-github-releases) | yes | yes | separate package |
| [`go install`](#with-go-install) | yes | yes | binary, not a package |
| [From source](#from-source) | yes | yes | yes |

## From GitHub Releases

Each [release](https://github.com/VaalaCat/openvaala/releases) has:

- **The `openvaala` binary** for each platform, with the web UI built in. Download the one for your system, make it executable and put it on your `PATH`:

  ```bash
  chmod +x openvaala-linux-amd64
  sudo mv openvaala-linux-amd64 /usr/local/bin/openvaala
  openvaala --version
  ```

- **Desktop packages** for macOS (a universal app for Apple silicon and Intel, macOS 12 or later) and Linux. The desktop app contains the full command-line tool as well; see [Desktop](../guides/desktop.md) for how to use it from a terminal and what Linux needs.

## With go install

If you have Go, this installs the command-line tool into `$(go env GOPATH)/bin`:

```bash
go install github.com/VaalaCat/openvaala/cmd/openvaala@latest
```

The same command with `cmd/openvaala-desktop` installs a separate binary named `openvaala-desktop`, also into `$(go env GOPATH)/bin`. Running it opens the desktop window, and it contains the same command-line tool. It is not the desktop package: no app bundle, disk image or menu entry. On Linux it still needs GTK 3, and WebKitGTK 4.1 for the WebView. See [Desktop](../guides/desktop.md).

```bash
go install github.com/VaalaCat/openvaala/cmd/openvaala-desktop@latest
openvaala-desktop                 # WebView window
openvaala-desktop app --native    # native interface
```

Published version tags include the built web UI, so `@latest` and an explicit release version of either binary work with the web UI, without Node.js or pnpm. The development branch keeps generated pages out of Git: `@main` does not include the web UI, so that desktop binary cannot open the WebView either. To run the web UI from a development checkout, build from source as described below. In a checkout of this repository, install the version you have with `go install ./cmd/openvaala` or `go install ./cmd/openvaala-desktop`.

Go 1.27.3 or later is required.

## From source

Building from source gives you the CLI with the web UI, and optionally the desktop app.

You need:

- Go 1.27.3 or later, and Git
- Node.js 24 or later and pnpm, for the web UI

The builds need no C toolchain. To run the desktop app on Linux you need GTK 3, plus WebKitGTK 4.1 for the WebView interface.

```bash
git clone https://github.com/VaalaCat/openvaala.git
cd openvaala
script/build.sh
```

`script/build.sh` builds the web UI, embeds it, and writes `dist/openvaala-<os>-<arch>` for your platform. Useful options:

- `--matrix` — build for Linux and macOS on amd64 and arm64.
- `--os linux --arch amd64,arm64` — choose targets.
- `--skip-frontend` — skip the web UI build and reuse an existing one (or build without the web UI).
- `--version v0.2.0` — set the version string; by default it is the nearest Git tag.
- `--output DIR` — write somewhere other than `dist/`.

To build the desktop app:

```bash
script/build-desktop.sh                          # macOS universal, Linux amd64 and arm64
script/build-desktop.sh --platform linux/amd64   # one target
```

The results are in `dist/desktop/`. A macOS `.dmg`, code signing and notarization need a macOS machine and the matching credentials; on other hosts the script still builds the `.app` and skips those steps (`--skip-dmg` and `--skip-notarize` skip them explicitly).

Run either script with `--help` for all options.

## Next

Continue with the [Quickstart](quickstart.md) to add a model and start your first conversation.
