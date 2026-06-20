//go:build linux

package main

/*
#cgo pkg-config: gtk+-3.0

#include <gtk/gtk.h>

static void ariacastMinimize(void* wnd) {
    gtk_window_iconify(GTK_WINDOW(wnd));
}
*/
import "C"

import (
	"bytes"
	"embed"
	"image"
	_ "image/png"
	"log"
	"os"

	"github.com/gen2brain/malgo"
	webview "github.com/webview/webview_go"
)

//go:embed ui/*
var uiFS embed.FS

// Platform Specific - Linux: Typically use PulseAudio Monitor for desktop audio
func getPlatformDeviceConfig() malgo.DeviceConfig {
	// On Linux, to capture "what you hear", you usually need to select the monitor source of the output.
	// Malgo/MiniAudio might auto-detect if we ask for Loopback, or we might need to pick the default input.
	// For simplicity in this cross-platform example, we will try the default capture device.
	// Advanced Linux users would use pavucontrol to redirect the monitor to this app's input.
	deviceConfig := malgo.DefaultDeviceConfig(malgo.Capture)
	deviceConfig.Capture.Format = AudioFormat
	deviceConfig.Capture.Channels = Channels
	deviceConfig.SampleRate = SampleRate
	deviceConfig.Alsa.NoMMap = 1
	return deviceConfig
}

// Window Methods specific to Linux
func (b *Binding) Minimize() {
	if w != nil {
		w.Dispatch(func() {
			C.ariacastMinimize(w.Window())
		})
	}
}

func (b *Binding) StartDrag() {
	// Dragging a borderless window from a custom header needs the live GDK
	// button-press event (coordinates + timestamp) which isn't available
	// anymore by the time this JS->Go binding call runs. Left as a no-op;
	// the window keeps its normal WM decorations so it can still be moved
	// by the OS title bar.
}

func main() {
	// WebKitGTK's accelerated compositing (used for things like the CSS
	// backdrop-filter blur in ui/index.html) is known to break pointer-event
	// hit-testing on some GPU/driver/Wayland combinations: the UI renders
	// fine but clicks and slider drags are silently swallowed. Disabling
	// compositing fixes that and costs nothing visually for this simple UI.
	// Respect an explicit override (e.g. set by the user in their shell).
	if _, set := os.LookupEnv("WEBKIT_DISABLE_COMPOSITING_MODE"); !set {
		os.Setenv("WEBKIT_DISABLE_COMPOSITING_MODE", "1")
	}

	debug := true
	w = webview.New(debug)
	defer w.Destroy()

	w.SetTitle("AriaCast - Linux Client")
	w.SetSize(450, 600, webview.HintNone)

	// Set the window icon from the embedded PNG so it shows in the taskbar,
	// window switcher (Alt+Tab), and the WM title bar.
	iconData, err := uiFS.ReadFile("ui/icon.png")
	if err != nil {
		log.Printf("Could not load icon: %v", err)
	} else if len(iconData) == 0 {
		log.Printf("Icon file is empty, skipping")
	} else {
		img, _, err := image.Decode(bytes.NewReader(iconData))
		if err != nil {
			log.Printf("Could not decode icon: %v", err)
		} else {
			w.Dispatch(func() {
				w.SetIcon(img, webview.IconKindDefault)
			})
		}
	}

	// Bindings
	w.Bind("startStream", func() { (&Binding{}).Start() })
	w.Bind("stopStream", func() { (&Binding{}).Stop() })
	w.Bind("setVolume", func(v float64) { (&Binding{}).SetVolume(v) })
	w.Bind("appMinimize", func() { (&Binding{}).Minimize() })
	w.Bind("appClose", func() { (&Binding{}).Close() })
	w.Bind("startDrag", func() { (&Binding{}).StartDrag() })

	// Linux might not support borderless dragging easily in webview without more complex GTK logic
	// So we keep standard decorations (HintNone) or use HintFixed if we want non-resizable.

	htmlContent, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		log.Fatal(err)
	}
	w.SetHtml(string(htmlContent))

	w.Run()
}
