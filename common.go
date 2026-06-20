package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/malgo"
	"github.com/gorilla/websocket"
	webview "github.com/webview/webview_go"
)

// Common Types and Globals

// Global App State
var (
	w          webview.WebView
	mu         sync.Mutex
	statusMsg  string  = "Idle"
	serverInfo string  = "Not Connected"
	volume     float64 = 1.0
	isRunning  bool
	cancelFunc context.CancelFunc
)

type Binding struct{}

func (b *Binding) Start() {
	mu.Lock()
	if isRunning {
		mu.Unlock()
		return
	}
	isRunning = true
	ctx, cancel := context.WithCancel(context.Background())
	cancelFunc = cancel
	mu.Unlock()

	setStatus("Connecting...")
	go runAudioLoop(ctx)
}

func (b *Binding) Stop() {
	mu.Lock()
	wasRunning := isRunning && cancelFunc != nil
	if wasRunning {
		cancelFunc()
		isRunning = false
	}
	mu.Unlock() // release before setStatus, which also locks mu

	if wasRunning {
		setStatus("Stopped")
	}
}

func (b *Binding) SetVolume(val float64) {
	mu.Lock()
	volume = val / 100.0
	mu.Unlock()
}

func (b *Binding) Close() {
	if w != nil {
		w.Terminate()
	}
	os.Exit(0)
}

func setStatus(msg string) {
	mu.Lock()
	statusMsg = msg
	mu.Unlock()

	log.Printf("[status] %s", msg)
	pushToUI("updateStatus", msg)
}

func setServerInfo(msg string) {
	mu.Lock()
	serverInfo = msg
	mu.Unlock()

	log.Printf("[server] %s", msg)
	pushToUI("updateServerInfo", msg)
}

// pushToUI calls a JS function in the page with a single string argument,
// from native Go code, via WebKit's "evaluate JavaScript" bridge. The
// argument is encoded with encoding/json (not naive string interpolation)
// so any character in msg - quotes, backslashes, unicode - is safely
// escaped and can never break out of the generated JS string literal.
func pushToUI(jsFunc, msg string) {
	if w == nil {
		return
	}
	encoded, err := json.Marshal(msg)
	if err != nil {
		log.Printf("pushToUI: failed to encode %q: %v", msg, err)
		return
	}
	w.Dispatch(func() {
		w.Eval(fmt.Sprintf("%s(%s);", jsFunc, encoded))
	})
}

// -----------------------------------------------------------------------------
// Audio Logic (Common)
// -----------------------------------------------------------------------------

const (
	DiscoveryPort     = 12888
	DefaultServerPort = 12889
	DiscoveryTimeout  = 2 * time.Second
	DiscoveryMsg      = "DISCOVER_AUDIOCAST"
	SampleRate        = 48000
	Channels          = 2
	AudioFormat       = malgo.FormatS16
	ProtocolFrameSize = 3840
)

type ServerInfo struct {
	ServerName string `json:"server_name"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
}

func runAudioLoop(ctx context.Context) {
	defer func() {
		mu.Lock()
		isRunning = false
		mu.Unlock()
		setStatus("Disconnected (Stopped)")
	}()

	// 1. Discovery
	setStatus("Scanning for server...")
	server, err := discoverServer(ctx)
	if err != nil {
		setStatus("Error: " + err.Error())
		return
	}
	setServerInfo(fmt.Sprintf("%s (%s:%d)", server.ServerName, server.IP, server.Port))
	setStatus("Server Found! Connecting...")

	// 2. Connect all three WebSockets required by the AriaCast protocol.
	// The Music Assistant receiver only considers the client "connected" and
	// starts its audio player once ALL three connections are established:
	//   /audio  – binary PCM frames (primary stream)
	//   /control – receives play/pause/stop commands from MA
	//   /stats   – buffer statistics (optional, but expected by the receiver)
	// Without /control and /stats the receiver stays silent even though
	// /audio traffic flows fine.
	base := fmt.Sprintf("ws://%s:%d", server.IP, server.Port)

	log.Printf("Connecting audio stream to %s/audio", base)
	audioConn, _, err := websocket.DefaultDialer.Dial(base+"/audio", nil)
	if err != nil {
		setStatus("Connection failed: " + err.Error())
		return
	}
	defer audioConn.Close()

	log.Printf("Connecting control channel to %s/control", base)
	ctrlConn, _, err := websocket.DefaultDialer.Dial(base+"/control", nil)
	if err != nil {
		log.Printf("Control channel failed (non-fatal): %v", err)
		// proceed without control – better than not connecting at all
	}
	if ctrlConn != nil {
		defer ctrlConn.Close()
	}

	log.Printf("Connecting stats channel to %s/stats", base)
	statsConn, _, err := websocket.DefaultDialer.Dial(base+"/stats", nil)
	if err != nil {
		log.Printf("Stats channel failed (non-fatal): %v", err)
	}
	if statsConn != nil {
		defer statsConn.Close()
	}

	// Alias for the audio connection used throughout the rest of the function
	c := audioConn

	setStatus("Streaming Active")

	// Read pump on /audio: the receiver sends a JSON handshake right after
	// connecting; gorilla/websocket only handles ping/pong while something
	// is actively reading, so without this loop dead connections go undetected.
	go func() {
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				log.Printf("/audio read loop ended: %v", err)
				return
			}
			if mt == websocket.TextMessage {
				log.Printf("Received from /audio: %s", msg)
			}
		}
	}()

	// Read pump on /control: forward play/pause/stop/etc. commands to log.
	// A full implementation would propagate these to the OS media session;
	// for now we at least keep the connection alive and log what MA sends.
	if ctrlConn != nil {
		go func() {
			for {
				mt, msg, err := ctrlConn.ReadMessage()
				if err != nil {
					log.Printf("/control read loop ended: %v", err)
					return
				}
				if mt == websocket.TextMessage {
					log.Printf("Control command from server: %s", msg)
				}
			}
		}()
	}

	// Read pump on /stats (server may push buffer info).
	if statsConn != nil {
		go func() {
			for {
				_, _, err := statsConn.ReadMessage()
				if err != nil {
					log.Printf("/stats read loop ended: %v", err)
					return
				}
			}
		}()
	}

	// 3. Notify the Music Assistant plugin that we are playing via HTTP POST to
	// /metadata. The plugin's __init__.py watches its /metadata WebSocket for
	// {"type":"metadata","data":{"is_playing":true,...}} and will NOT start an
	// audio player until it sees this. Without this POST the connection looks
	// healthy from the outside (WebSocket traffic flows on all three channels)
	// but MA stays silent because its trigger is never fired.
	//
	// The binary converts camelCase fields from the POST body to snake_case
	// before re-broadcasting on the /metadata WebSocket to the plugin.
	httpBase := fmt.Sprintf("http://%s:%d", server.IP, server.Port)
	sendMetadata := func(playing bool) {
		body := fmt.Sprintf(
			`{"data":{"title":"AriaCast","artist":"Desktop Audio","album":"",`+
				`"artworkUrl":"","durationMs":0,"positionMs":0,"isPlaying":%v}}`,
			playing,
		)
		resp, err := http.Post(
			httpBase+"/metadata",
			"application/json",
			strings.NewReader(body),
		)
		if err != nil {
			log.Printf("Metadata POST error: %v", err)
			return
		}
		resp.Body.Close()
		log.Printf("Metadata POST (isPlaying=%v) → %s", playing, resp.Status)
	}

	sendMetadata(true)

	// Repeat every 10 s while streaming to keep the plugin's state alive.
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				sendMetadata(false)
				return
			case <-ticker.C:
				sendMetadata(true)
			}
		}
	}()

	// 4. Audio Capture Setup
	mctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(message string) {
		// Log
	})
	if err != nil {
		setStatus("Audio Init Error: " + err.Error())
		return
	}
	defer mctx.Free()

	// Platform Specific Device Logic
	deviceConfig := getPlatformDeviceConfig()

	// Log available capture devices for diagnostics.
	// We intentionally do NOT set deviceConfig.Capture.DeviceID here: pinning
	// an explicit device ID causes miniaudio to open the PulseAudio stream
	// with PA_STREAM_DONT_MOVE, which locks the source and prevents pavucontrol
	// from moving it to a different source while streaming. By leaving DeviceID
	// unset (= default source), the stream stays freely movable and the user
	// can pick the right source (e.g. "Monitor of <card>") live in pavucontrol's
	// Recording tab without stopping the stream.
	if devices, derr := mctx.Context.Devices(malgo.Capture); derr == nil {
		for i := range devices {
			d := devices[i]
			tag := ""
			if d.IsDefault != 0 {
				tag = " [default]"
			}
			log.Printf("Capture device found: %q%s", d.Name(), tag)
		}
		log.Printf("Streaming from the PulseAudio default source. " +
			"Use pavucontrol → Recording tab to switch the source live, " +
			"or set ARIACAST_CAPTURE_DEVICE=<partial name> to pin one at startup.")
	} else {
		log.Printf("Could not enumerate capture devices: %v", derr)
	}

	// Double buffering logic to handle frames
	frameBuffer := make([]byte, 0, ProtocolFrameSize*2)
	var bufferMutex sync.Mutex

	onRecvFrames := func(pOutputSample, pInputSamples []byte, framecount uint32) {
		bufferMutex.Lock()
		defer bufferMutex.Unlock()

		// Apply gain
		mu.Lock()
		currentGain := volume
		mu.Unlock()

		if currentGain != 1.0 {
			// pInputSamples is raw byte slice (S16LE).
			for i := 0; i < len(pInputSamples); i += 2 {
				if i+1 >= len(pInputSamples) {
					break
				}
				val := int16(binary.LittleEndian.Uint16(pInputSamples[i : i+2]))
				fVal := float64(val) * currentGain
				if fVal > 32767 {
					val = 32767
				} else if fVal < -32768 {
					val = -32768
				} else {
					val = int16(fVal)
				}
				binary.LittleEndian.PutUint16(pInputSamples[i:i+2], uint16(val))
			}
		}

		frameBuffer = append(frameBuffer, pInputSamples...)

		// Send chunks of exactly ProtocolFrameSize
		for len(frameBuffer) >= ProtocolFrameSize {
			chunk := frameBuffer[:ProtocolFrameSize]
			err := c.WriteMessage(websocket.BinaryMessage, chunk)
			if err != nil {
				// We don't return here to keep capture unless fatal
				log.Printf("WebSocket write failed: %v", err)
				return
			}
			frameBuffer = frameBuffer[ProtocolFrameSize:]
		}
	}

	deviceCallbacks := malgo.DeviceCallbacks{
		Data: onRecvFrames,
	}

	device, err := malgo.InitDevice(mctx.Context, deviceConfig, deviceCallbacks)
	if err != nil {
		setStatus("Device Init Error: " + err.Error())
		return
	}
	defer device.Uninit()

	if err := device.Start(); err != nil {
		setStatus("Start Error: " + err.Error())
		return
	}

	// Wait until context cancelled
	<-ctx.Done()
}

func discoverServer(ctx context.Context) (*ServerInfo, error) {
	pc, err := net.ListenPacket("udp4", ":0") // Bind to random port
	if err != nil {
		return nil, err
	}
	defer pc.Close()

	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("255.255.255.255:%d", DiscoveryPort))
	if err != nil {
		return nil, err
	}

	// Send discovery packet
	if _, err := pc.WriteTo([]byte(DiscoveryMsg), addr); err != nil {
		return nil, err
	}

	// Read loop
	buf := make([]byte, 1024)
	resultChan := make(chan *ServerInfo, 1)

	go func() {
		pc.SetReadDeadline(time.Now().Add(DiscoveryTimeout))
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}

		var info ServerInfo
		if err := json.Unmarshal(buf[:n], &info); err == nil {
			resultChan <- &info
		}
	}()

	select {
	case info := <-resultChan:
		return info, nil
	case <-time.After(DiscoveryTimeout):
		return nil, fmt.Errorf("timeout looking for server")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
