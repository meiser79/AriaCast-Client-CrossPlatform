# AriaCast Client

A modern, cross-platform client for AriaCast built with Go and Webview.

## Dependencies

- Go 1.25+
- **Windows**: `gcc` (MinGW-w64 via MSYS2 recommended)
- **Linux**: `libgtk-3-dev`, `libwebkit2gtk-4.1-dev`
- **macOS**: Xcode Command Line Tools

## Building

### Windows

1.  **Install MinGW-w64**: Ensure `gcc` is in your PATH.
    ```powershell
    # Verify GCC
    gcc --version
    ```
2.  **Generate Icon Resource** (Optional):
    ```powershell
    # Requires 'rsrc' tool: go install github.com/akavel/rsrc@latest
    rsrc -ico icon.ico -o rsrc.syso
    ```
3.  **Build**:
    ```powershell
    go build -tags "cgo" -ldflags="-H windowsgui" -o AriaCastClient.exe .
    ```

### Linux

1.  **Install Dependencies**:
    ```bash
    sudo apt install libgtk-3-dev libwebkit2gtk-4.1-dev
    ```
    > Modern distros (Ubuntu 24.04+, Debian 13+, Fedora 40+, Arch, etc.) only ship `webkit2gtk-4.1`; the older `webkit2gtk-4.0` package has been removed/deprecated upstream. This project vendors a small patched copy of `webview/webview_go` under `third_party/webview_go` (wired up via a `replace` directive in `go.mod`) so it links against `webkit2gtk-4.1` instead of the unmaintained `4.0` build.
    > **Known issue / fixed by default:** on some GPU driver / Wayland setups, WebKitGTK's accelerated compositing (used for the CSS `backdrop-filter` blur) silently breaks mouse hit-testing — the UI renders fine but buttons and the slider don't react to clicks. `main_linux.go` now sets `WEBKIT_DISABLE_COMPOSITING_MODE=1` by default before the webview is created to avoid this. If you need compositing for some reason, export `WEBKIT_DISABLE_COMPOSITING_MODE=0` yourself before launching to override the default.
2.  **Build**:
    ```bash
    go build -o AriaCastClient .
    ```

### macOS

1.  **Build**:
    ```bash
    go build -o AriaCastClient .
    ```

## Usage

1.  Start the **AriaCast Server** on the host machine.
2.  Run the **AriaCast Client**.
3.  The client will automatically discover the server on the local network.
4.  **Windows**: For best results, install **VB-Cable** and set it as your default playback device to stream system audio silently.


