package deskstream

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// writeWait is the deadline for a single WebSocket write.
	writeWait = 10 * time.Second
	// pongWait / pingPeriod keep the connection alive and detect dead peers.
	pongWait   = 60 * time.Second
	pingPeriod = 25 * time.Second
)

// handleStream upgrades an HTTP request to a WebSocket and serves the H.264
// stream per the wire contract:
//
//  1. The server sends ONE TEXT frame: the init JSON (see InitMessage).
//  2. The server then sends BINARY frames, each a sequence of Annex-B NAL
//     units. SPS+PPS are prepended on every IDR keyframe (see nalFramer).
//  3. The client MAY send a TEXT frame {"type":"requestKeyframe"} to force an
//     immediate IDR; the server restarts its encoder to honor it (a fresh
//     ffmpeg process always begins with SPS+PPS+IDR).
//
// A fresh connection is treated as an implicit keyframe request: the encoder is
// started from scratch, so the first BINARY frame already carries SPS+PPS+IDR
// and the client can decode without waiting up to the 2s periodic interval.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Printf("websocket upgrade failed for %s: %v", r.RemoteAddr, err)
		return
	}
	atomic.AddInt64(&s.totalConns, 1)
	atomic.AddInt64(&s.activeConns, 1)
	remote := r.RemoteAddr
	s.logger.Printf("client connected: %s", remote)
	defer func() {
		conn.Close()
		atomic.AddInt64(&s.activeConns, -1)
		s.logger.Printf("client disconnected: %s", remote)
	}()

	display := resolveDisplay()
	enc, err := s.encoder()
	if err != nil {
		s.logger.Printf("no encoder available: %v", err)
		return
	}
	geom := detectGeometry(display)
	gop := s.fps * 2

	cfg := EncodeConfig{
		Display:          display,
		Width:            geom.Width,
		Height:           geom.Height,
		FPS:              s.fps,
		KeyframeInterval: gop,
	}

	// Send the init TEXT frame first (wire contract step 1).
	initMsg := NewInitMessage(geom.Width, geom.Height, s.fps, gop)
	initBytes, err := initMsg.Marshal()
	if err != nil {
		s.logger.Printf("marshal init: %v", err)
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := conn.WriteMessage(websocket.TextMessage, initBytes); err != nil {
		s.logger.Printf("write init failed: %v", err)
		return
	}

	// Single writer: all WebSocket writes happen on this goroutine via the
	// payload/ping channels. gorilla panics on concurrent writes, so the encoder
	// callback (a different goroutine) only ENQUEUES; it never writes directly.
	payloads := make(chan []byte, 64)
	restart := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	runner := newEncoderRunner(cfg, enc, func(p []byte) {
		// Drop frames if the client cannot keep up rather than block ffmpeg.
		select {
		case payloads <- p:
		default:
		}
	})
	if err := runner.Start(ctx); err != nil {
		s.logger.Printf("encoder start failed: %v", err)
		return
	}
	// Stop the CURRENT runner at return. The closure captures the runner
	// VARIABLE (evaluated at return time), not the value bound here, so it
	// correctly stops whichever runner is live after a requestKeyframe restart
	// reassigned it — otherwise the restarted ffmpeg would never be reaped.
	defer func() { runner.Stop() }()

	// Reader goroutine: handles client TEXT frames (requestKeyframe) and detects
	// disconnects. It never writes to the socket.
	go func() {
		conn.SetReadLimit(4096)
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(pongWait))
		})
		for {
			mt, data, rerr := conn.ReadMessage()
			if rerr != nil {
				cancel()
				return
			}
			if mt == websocket.TextMessage && parseClientMessage(data) == ClientMsgRequestKeyframe {
				select {
				case restart <- struct{}{}:
				default:
				}
			}
		}
	}()

	ping := time.NewTicker(pingPeriod)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-restart:
			// On-demand keyframe: restart the encoder so the next frames begin
			// with SPS+PPS+IDR. Drain any stale queued payloads first so the
			// client does not briefly receive pre-restart P-frames. A late
			// onPayload from the old runner's read goroutine can still slip one
			// stale P-frame past drain before the new keyframe arrives; the
			// decoder self-corrects within a frame, which is acceptable here.
			runner.Stop()
			drain(payloads)
			runner = newEncoderRunner(cfg, enc, func(p []byte) {
				select {
				case payloads <- p:
				default:
				}
			})
			if err := runner.Start(ctx); err != nil {
				s.logger.Printf("encoder restart failed: %v", err)
				return
			}
		case p := <-payloads:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
				return
			}
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// drain empties a payload channel without blocking.
func drain(ch chan []byte) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
