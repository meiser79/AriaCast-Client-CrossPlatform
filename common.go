package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
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
	defer mu.Unlock()
	if isRunning && cancelFunc != nil {
		cancelFunc() // Cancel context
		isRunning = false
	}
	setStatus("Stopped")
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

	if w != nil {
		w.Dispatch(func() {
			// Simple escape for single quotes
			// full json encoding would be safer but this is lightweight
			js := fmt.Sprintf("updateStatus('%s');", msg)
			w.Eval(js)
		})
	}
}

func setServerInfo(msg string) {
	mu.Lock()
	serverInfo = msg
	mu.Unlock()

	if w != nil {
		w.Dispatch(func() {
			js := fmt.Sprintf("updateServerInfo('%s');", msg)
			w.Eval(js)
		})
	}
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

	// 2. Connect WebSocket - FIXED PATH
	u := fmt.Sprintf("ws://%s:%d/audio", server.IP, server.Port)
	log.Printf("Connecting to %s", u)

	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		setStatus("Connection failed: " + err.Error())
		return
	}
	defer c.Close()

	setStatus("Streaming Active")

	// 3. Audio Capture Setup
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
