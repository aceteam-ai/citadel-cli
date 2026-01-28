// Package demo provides a simple HTTP server for demonstrating node connectivity.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/platform"
)

// NodeInfo represents the node's status for the demo page
type NodeInfo struct {
	Hostname     string   `json:"hostname"`
	Platform     string   `json:"platform"`
	Arch         string   `json:"arch"`
	Version      string   `json:"version"`
	Connected    bool     `json:"connected"`
	NetworkIP    string   `json:"network_ip,omitempty"`
	Services     []string `json:"services,omitempty"`
	GPUName      string   `json:"gpu_name,omitempty"`
	GPUMemory    string   `json:"gpu_memory,omitempty"`
	StartedAt    string   `json:"started_at"`
}

// Server is the demo HTTP server
type Server struct {
	port       int
	version    string
	getInfo    func() NodeInfo
	server     *http.Server
	startedAt  time.Time
}

// NewServer creates a new demo server
func NewServer(port int, version string, getInfo func() NodeInfo) *Server {
	return &Server{
		port:      port,
		version:   version,
		getInfo:   getInfo,
		startedAt: time.Now(),
	}
}

// Start starts the demo server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/info", s.handleAPI)
	mux.HandleFunc("/health", s.handleHealth)

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	errChan := make(chan error, 1)
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.server.Shutdown(shutdownCtx)
	}
}

// Stop stops the demo server
func (s *Server) Stop() error {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.server.Shutdown(ctx)
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	info := s.getNodeInfo()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func (s *Server) getNodeInfo() NodeInfo {
	if s.getInfo != nil {
		info := s.getInfo()
		info.StartedAt = s.startedAt.Format(time.RFC3339)
		return info
	}

	// Default info if no callback provided
	hostname, _ := os.Hostname()
	info := NodeInfo{
		Hostname:  hostname,
		Platform:  runtime.GOOS,
		Arch:      runtime.GOARCH,
		Version:   s.version,
		StartedAt: s.startedAt.Format(time.RFC3339),
	}

	// Try to get GPU info
	if detector, err := platform.GetGPUDetector(); err == nil && detector.HasGPU() {
		if gpus, err := detector.GetGPUInfo(); err == nil && len(gpus) > 0 {
			info.GPUName = gpus[0].Name
			info.GPUMemory = gpus[0].Memory
		}
	}

	return info
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	info := s.getNodeInfo()

	tmpl := template.Must(template.New("index").Parse(indexHTML))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, info)
}

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Citadel Node - {{.Hostname}}</title>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            background: linear-gradient(135deg, #1a1a2e 0%, #16213e 100%);
            color: #e0e0e0;
            min-height: 100vh;
            display: flex;
            align-items: center;
            justify-content: center;
            padding: 20px;
        }
        .card {
            background: rgba(255, 255, 255, 0.05);
            border: 1px solid rgba(255, 255, 255, 0.1);
            border-radius: 16px;
            padding: 40px;
            max-width: 500px;
            width: 100%;
            backdrop-filter: blur(10px);
        }
        .header {
            text-align: center;
            margin-bottom: 30px;
        }
        .logo {
            font-size: 48px;
            margin-bottom: 10px;
        }
        h1 {
            font-size: 24px;
            font-weight: 600;
            color: #fff;
        }
        .hostname {
            color: #4ecdc4;
            font-family: monospace;
        }
        .status {
            display: flex;
            align-items: center;
            justify-content: center;
            gap: 8px;
            margin-top: 10px;
            font-size: 14px;
        }
        .status-dot {
            width: 10px;
            height: 10px;
            border-radius: 50%;
            background: {{if .Connected}}#4ecdc4{{else}}#666{{end}};
            animation: {{if .Connected}}pulse 2s infinite{{else}}none{{end}};
        }
        @keyframes pulse {
            0%, 100% { opacity: 1; }
            50% { opacity: 0.5; }
        }
        .info {
            margin-top: 30px;
        }
        .info-row {
            display: flex;
            justify-content: space-between;
            padding: 12px 0;
            border-bottom: 1px solid rgba(255, 255, 255, 0.1);
        }
        .info-row:last-child { border-bottom: none; }
        .label { color: #888; }
        .value { color: #fff; font-family: monospace; }
        .footer {
            margin-top: 30px;
            text-align: center;
            font-size: 12px;
            color: #666;
        }
        .footer a { color: #4ecdc4; text-decoration: none; }
    </style>
</head>
<body>
    <div class="card">
        <div class="header">
            <div class="logo">🏰</div>
            <h1>Citadel Node</h1>
            <div class="hostname">{{.Hostname}}</div>
            <div class="status">
                <span class="status-dot"></span>
                <span>{{if .Connected}}Connected{{else}}Offline{{end}}</span>
            </div>
        </div>

        <div class="info">
            <div class="info-row">
                <span class="label">Platform</span>
                <span class="value">{{.Platform}}/{{.Arch}}</span>
            </div>
            {{if .NetworkIP}}
            <div class="info-row">
                <span class="label">Network IP</span>
                <span class="value">{{.NetworkIP}}</span>
            </div>
            {{end}}
            {{if .GPUName}}
            <div class="info-row">
                <span class="label">GPU</span>
                <span class="value">{{.GPUName}}</span>
            </div>
            {{end}}
            {{if .GPUMemory}}
            <div class="info-row">
                <span class="label">GPU Memory</span>
                <span class="value">{{.GPUMemory}}</span>
            </div>
            {{end}}
            <div class="info-row">
                <span class="label">Version</span>
                <span class="value">{{.Version}}</span>
            </div>
            <div class="info-row">
                <span class="label">Started</span>
                <span class="value">{{.StartedAt}}</span>
            </div>
        </div>

        <div class="footer">
            Powered by <a href="https://aceteam.ai">AceTeam.ai</a>
        </div>
    </div>
</body>
</html>`
