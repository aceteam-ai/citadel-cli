package jobs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aceteam-ai/citadel-cli/internal/config"
	"github.com/aceteam-ai/citadel-cli/internal/nexus"
)

type AMTKVMHandler struct {
	Origin      func() string
	DeviceCreds func() config.DeviceCreds
	HTTPClient  *http.Client
}

// ErrAMTOutcomeUnknown means a mutating command might have reached the target.
// The runner reports this without retrying the one-use action.
var ErrAMTOutcomeUnknown = errors.New("AMT action outcome unknown; verify the target before another command")

// A fixed lock set bounds memory while serializing every operation to the
// same AMT host. Hash collisions only serialize unrelated hosts.
var amtTargetLocks [64]sync.Mutex

func amtTargetLock(host string) *sync.Mutex {
	hash := uint64(14695981039346656037)
	for i := 0; i < len(host); i++ {
		hash ^= uint64(host[i])
		hash *= 1099511628211
	}
	return &amtTargetLocks[hash%uint64(len(amtTargetLocks))]
}

type amtCredential struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (h *AMTKVMHandler) credential(ctx context.Context, job *nexus.Job) (amtCredential, error) {
	var credential amtCredential
	host := job.Payload["host"]
	if !amtLANAddress(host) || h.Origin == nil {
		return credential, errors.New("AMT binding unavailable")
	}
	endpoint := job.Payload["credential_url"]
	configured := strings.TrimRight(h.Origin(), "/")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || configured == "" || endpoint != configured+"/api/fabric/amt/credential" {
		return credential, errors.New("AMT credential origin unavailable")
	}
	token := job.Payload["credential_token"]
	if len(token) != 43 {
		return credential, errors.New("AMT credential capability unavailable")
	}
	for _, c := range token {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return credential, errors.New("AMT credential capability unavailable")
		}
	}
	loadCreds := h.DeviceCreds
	if loadCreds == nil {
		loadCreds = config.LoadDeviceCredsConverged
	}
	creds := loadCreds()
	if creds.Token == "" || strings.TrimRight(creds.APIBaseURL, "/") != configured {
		return credential, errors.New("AMT enrolled device identity unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return credential, errors.New("AMT credential request unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+creds.Token)
	req.Header.Set("X-AMT-Capability", token)
	client := h.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return credential, errors.New("AMT credential delivery failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return credential, errors.New("AMT credential delivery refused")
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&credential); err != nil {
		return amtCredential{}, errors.New("AMT credential response invalid")
	}
	if credential.Host != host || credential.Username == "" || credential.Password == "" {
		return amtCredential{}, errors.New("AMT credential binding mismatch")
	}
	return credential, nil
}

func (h *AMTKVMHandler) Execute(ctx JobContext, job *nexus.Job) ([]byte, error) {
	op := job.Payload["operation"]
	if op != "status" && op != "screenshot" && op != "keys" && op != "type" && op != "power" {
		return nil, errors.New("unsupported AMT operation")
	}
	if op == "power" {
		action := job.Payload["action"]
		if action != "on" && action != "off" && action != "cycle" && action != "reset" && action != "boot_to_setup" {
			return nil, errors.New("unsupported AMT power action")
		}
		if job.Payload["confirm_target"] == "" || job.Payload["confirm_target"] != job.Payload["host"] || job.Payload["confirm_action"] == "" || job.Payload["confirm_action"] != action {
			return nil, errors.New("AMT power requires explicit target and action confirmation")
		}
	}
	lock := amtTargetLock(job.Payload["host"])
	lock.Lock()
	defer lock.Unlock()
	credential, err := h.credential(ctx.Context(), job)
	if err != nil {
		return nil, err
	}
	wsman, err := newAMTWSMan(credential.Host, credential.Username, credential.Password)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	switch op {
	case "status":
		result, err = wsman.status(ctx.Context())
	case "power":
		action := job.Payload["action"]
		if action == "boot_to_setup" {
			err = wsman.bootToSetup(ctx.Context())
		} else {
			err = wsman.power(ctx.Context(), action)
		}
		result = map[string]any{"ok": true}
	case "screenshot", "keys", "type":
		var conn net.Conn
		conn, err = connectAMTRedirection(ctx.Context(), credential.Host, credential.Username, credential.Password)
		if err == nil {
			defer conn.Close()
			var rfb *amtRFB
			rfb, err = newAMTRFB(conn)
			if err == nil {
				switch op {
				case "screenshot":
					var pngBytes []byte
					pngBytes, err = rfb.screenshot()
					if err == nil {
						result = map[string]any{"image": base64.StdEncoding.EncodeToString(pngBytes), "encoding": "base64", "format": "png", "width": strconv.Itoa(rfb.width), "height": strconv.Itoa(rfb.height)}
					}
				case "keys":
					var count int
					count, err = rfb.keys(ctx.Context(), job.Payload["keys"])
					result = map[string]any{"events_sent": count}
				case "type":
					err = rfb.typeText(ctx.Context(), job.Payload["text"])
					result = map[string]any{"typed": len(job.Payload["text"])}
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func readN(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	if err != nil {
		return nil, errors.New("AMT redirection connection closed")
	}
	return buf, nil
}

func writeAll(conn net.Conn, data []byte) error {
	_, err := conn.Write(data)
	if err != nil {
		return errors.New("AMT redirection write failed")
	}
	return nil
}

func lenByte(data []byte) []byte { return append([]byte{byte(len(data))}, data...) }

func redirectionReply(conn net.Conn) (byte, byte, []byte, error) {
	header, err := readN(conn, 9)
	if err != nil {
		return 0, 0, nil, err
	}
	length := binary.LittleEndian.Uint32(header[5:9])
	if length > 4096 {
		return 0, 0, nil, errors.New("AMT authentication reply too large")
	}
	data, err := readN(conn, int(length))
	return header[1], header[4], data, err
}

func connectAMTRedirection(ctx context.Context, host, username, password string) (net.Conn, error) {
	if !amtLANAddress(host) {
		return nil, errors.New("AMT target must be on a private LAN")
	}
	if len(username) > 255 {
		return nil, errors.New("AMT username invalid")
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, "16994"))
	if err != nil {
		return nil, errors.New("AMT redirection connection failed")
	}
	closeOnError := func(e error) (net.Conn, error) { conn.Close(); return nil, e }
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err = writeAll(conn, []byte{0x10, 0, 0, 0, 'K', 'V', 'M', 'R'}); err != nil {
		return closeOnError(err)
	}
	start, err := readN(conn, 13)
	if err != nil {
		return closeOnError(err)
	}
	if start[0] != 0x11 || start[1] != 0 {
		return closeOnError(errors.New("AMT redirection session refused"))
	}
	if _, err = readN(conn, int(start[12])); err != nil {
		return closeOnError(err)
	}
	if err = writeAll(conn, []byte{0x13, 0, 0, 0, 0, 0, 0, 0, 0}); err != nil {
		return closeOnError(err)
	}
	status, _, methods, err := redirectionReply(conn)
	if err != nil {
		return closeOnError(err)
	}
	if status != 0 || !bytes.Contains(methods, []byte{4}) {
		return closeOnError(errors.New("AMT Digest redirection authentication unavailable"))
	}
	user, uri := []byte(username), []byte("/RedirectionService")
	query := []byte{0x13, 0, 0, 0, 4, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(query[5:], uint32(len(user)+len(uri)+8))
	query = append(query, lenByte(user)...)
	query = append(query, 0, 0)
	query = append(query, lenByte(uri)...)
	query = append(query, 0, 0, 0, 0)
	if err = writeAll(conn, query); err != nil {
		return closeOnError(err)
	}
	status, _, challenge, err := redirectionReply(conn)
	if err != nil {
		return closeOnError(err)
	}
	if status != 0 {
		return closeOnError(errors.New("AMT redirection challenge refused"))
	}
	fields := make([][]byte, 0, 3)
	for i := 0; i < len(challenge) && len(fields) < 3; {
		n := int(challenge[i])
		i++
		if i+n > len(challenge) {
			return closeOnError(errors.New("AMT redirection challenge invalid"))
		}
		fields = append(fields, challenge[i:i+n])
		i += n
	}
	if len(fields) != 3 {
		return closeOnError(errors.New("AMT redirection challenge invalid"))
	}
	realm, nonce, qop := fields[0], fields[1], fields[2]
	cnonceBytes := make([]byte, 16)
	if _, err = rand.Read(cnonceBytes); err != nil {
		return closeOnError(errors.New("AMT authentication unavailable"))
	}
	cnonce := []byte(hex.EncodeToString(cnonceBytes))
	nc := []byte("00000002")
	inner := md5Hex(username + ":" + string(realm) + ":" + password)
	digest := []byte(md5Hex(inner + ":" + string(nonce) + ":" + string(nc) + ":" + string(cnonce) + ":" + string(qop) + ":" + md5Hex("POST:/RedirectionService")))
	parts := [][]byte{user, realm, nonce, uri, cnonce, nc, digest, qop}
	length := 0
	for _, p := range parts {
		if len(p) > 255 {
			return closeOnError(errors.New("AMT Digest field too long"))
		}
		length += 1 + len(p)
	}
	auth := []byte{0x13, 0, 0, 0, 4, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(auth[5:], uint32(length))
	for _, p := range parts {
		auth = append(auth, lenByte(p)...)
	}
	if err = writeAll(conn, auth); err != nil {
		return closeOnError(err)
	}
	status, _, _, err = redirectionReply(conn)
	if err != nil {
		return closeOnError(err)
	}
	if status != 0 {
		return closeOnError(errors.New("AMT redirection authentication failed"))
	}
	if err = writeAll(conn, []byte{0x40, 0, 0, 0, 0, 0, 0, 0}); err != nil {
		return closeOnError(err)
	}
	channel, err := readN(conn, 8)
	if err != nil {
		return closeOnError(err)
	}
	if channel[0] != 0x41 {
		return closeOnError(errors.New("AMT KVM channel refused"))
	}
	return conn, nil
}

type amtRFB struct {
	conn            net.Conn
	width, height   int
	mutationStarted bool
}

func newAMTRFB(conn net.Conn) (*amtRFB, error) {
	version, err := readN(conn, 12)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(version, []byte("RFB 003.")) {
		return nil, errors.New("AMT RFB version invalid")
	}
	if err = writeAll(conn, []byte("RFB 003.008\n")); err != nil {
		return nil, err
	}
	count, err := readN(conn, 1)
	if err != nil {
		return nil, err
	}
	if count[0] == 0 {
		return nil, errors.New("AMT RFB security unavailable")
	}
	types, err := readN(conn, int(count[0]))
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(types, []byte{1}) {
		return nil, errors.New("AMT RFB security None unavailable")
	}
	if err = writeAll(conn, []byte{1}); err != nil {
		return nil, err
	}
	status, err := readN(conn, 4)
	if err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(status) != 0 {
		return nil, errors.New("AMT RFB security refused")
	}
	if err = writeAll(conn, []byte{1}); err != nil {
		return nil, err
	}
	server, err := readN(conn, 24)
	if err != nil {
		return nil, err
	}
	w, h := int(binary.BigEndian.Uint16(server[:2])), int(binary.BigEndian.Uint16(server[2:4]))
	if w < 1 || h < 1 || w > 4096 || h > 2160 {
		return nil, errors.New("AMT RFB dimensions invalid")
	}
	nameLength := binary.BigEndian.Uint32(server[20:24])
	if nameLength > 4096 {
		return nil, errors.New("AMT RFB server name too long")
	}
	if _, err = readN(conn, int(nameLength)); err != nil {
		return nil, err
	}
	// Set RGB565 little-endian and request only raw rectangles.
	pixel := []byte{0, 0, 0, 0, 16, 16, 0, 1, 0, 31, 0, 63, 0, 31, 11, 5, 0, 0, 0, 0}
	if err = writeAll(conn, pixel); err != nil {
		return nil, err
	}
	if err = writeAll(conn, []byte{2, 0, 0, 1, 0, 0, 0, 0}); err != nil {
		return nil, err
	}
	return &amtRFB{conn: conn, width: w, height: h}, nil
}

// Each key write gets its own bounded I/O window. The initial redirection
// handshake deadline must not cut off a valid paced 4096-byte input at 30s.
func (r *amtRFB) key(ctx context.Context, sym uint32, down bool) error {
	if err := ctx.Err(); err != nil {
		if r.mutationStarted {
			return ErrAMTOutcomeUnknown
		}
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := r.conn.SetWriteDeadline(deadline); err != nil {
		if r.mutationStarted {
			return ErrAMTOutcomeUnknown
		}
		return errors.New("AMT redirection write deadline unavailable")
	}
	// Cancellation must also interrupt a blocked write, not only pacing.
	stop := context.AfterFunc(ctx, func() { _ = r.conn.Close() })
	defer stop()

	packet := make([]byte, 8)
	packet[0] = 4
	if down {
		packet[1] = 1
	}
	binary.BigEndian.PutUint32(packet[4:], sym)
	n, err := r.conn.Write(packet)
	if n > 0 {
		r.mutationStarted = true
	}
	if err != nil || n != len(packet) {
		if r.mutationStarted {
			return ErrAMTOutcomeUnknown
		}
		return errors.New("AMT redirection write failed")
	}
	return nil
}

var amtKeysyms = map[string]uint32{
	"enter": 0xff0d, "esc": 0xff1b, "escape": 0xff1b, "tab": 0xff09, "backspace": 0xff08,
	"delete": 0xffff, "del": 0xffff, "space": 0x20, "up": 0xff52, "down": 0xff54,
	"left": 0xff51, "right": 0xff53, "home": 0xff50, "end": 0xff57,
	"pgup": 0xff55, "pageup": 0xff55, "pgdn": 0xff56, "pagedown": 0xff56,
	"ctrl": 0xffe3, "control": 0xffe3, "alt": 0xffe9, "shift": 0xffe1,
}

func amtKeysym(token string) (uint32, error) {
	lower := strings.ToLower(strings.TrimSpace(token))
	if value, ok := amtKeysyms[lower]; ok {
		return value, nil
	}
	if strings.HasPrefix(lower, "f") {
		n, err := strconv.Atoi(lower[1:])
		if err == nil && n >= 1 && n <= 12 {
			return uint32(0xffbe + n - 1), nil
		}
	}
	if len(token) == 1 && token[0] >= 32 && token[0] <= 126 {
		return uint32(token[0]), nil
	}
	return 0, errors.New("unsupported AMT key")
}

func (r *amtRFB) keys(ctx context.Context, combo string) (int, error) {
	parts := strings.Split(combo, "+")
	if len(parts) == 0 || len(parts) > 8 || combo == "" {
		return 0, errors.New("AMT key combination invalid")
	}
	syms := make([]uint32, 0, len(parts))
	for _, part := range parts {
		sym, err := amtKeysym(part)
		if err != nil {
			return 0, err
		}
		syms = append(syms, sym)
	}
	for _, sym := range syms {
		if err := r.key(ctx, sym, true); err != nil {
			return 0, err
		}
	}
	if err := amtInputPause(ctx, 50*time.Millisecond); err != nil {
		if r.mutationStarted {
			return 0, ErrAMTOutcomeUnknown
		}
		return 0, err
	}
	for i := len(syms) - 1; i >= 0; i-- {
		if err := r.key(ctx, syms[i], false); err != nil {
			return 0, err
		}
	}
	return len(syms) * 2, nil
}

func (r *amtRFB) typeText(ctx context.Context, value string) error {
	if value == "" || len(value) > 4096 {
		return errors.New("AMT text length invalid")
	}
	for _, c := range value {
		if c != '\n' && c != '\t' && (c < 32 || c > 126) {
			return errors.New("AMT text must be ASCII")
		}
	}
	for _, c := range value {
		sym := uint32(c)
		if c == '\n' {
			sym = 0xff0d
		}
		if c == '\t' {
			sym = 0xff09
		}
		if err := r.key(ctx, sym, true); err != nil {
			return err
		}
		if err := r.key(ctx, sym, false); err != nil {
			return err
		}
		if err := amtInputPause(ctx, 30*time.Millisecond); err != nil {
			if r.mutationStarted {
				return ErrAMTOutcomeUnknown
			}
			return err
		}
	}
	return nil
}

func amtInputPause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func (r *amtRFB) screenshot() ([]byte, error) {
	request := make([]byte, 10)
	request[0] = 3
	binary.BigEndian.PutUint16(request[6:], uint16(r.width))
	binary.BigEndian.PutUint16(request[8:], uint16(r.height))
	if err := writeAll(r.conn, request); err != nil {
		return nil, err
	}
	frame := make([]byte, r.width*r.height*2)
	for messages := 0; messages < 64; messages++ {
		typeByte, err := readN(r.conn, 1)
		if err != nil {
			return nil, err
		}
		switch typeByte[0] {
		case 0:
			header, err := readN(r.conn, 3)
			if err != nil {
				return nil, err
			}
			rects := int(binary.BigEndian.Uint16(header[1:]))
			if rects > 256 {
				return nil, errors.New("AMT RFB rectangle count invalid")
			}
			for i := 0; i < rects; i++ {
				rect, err := readN(r.conn, 12)
				if err != nil {
					return nil, err
				}
				x, y, w, h := int(binary.BigEndian.Uint16(rect[:2])), int(binary.BigEndian.Uint16(rect[2:4])), int(binary.BigEndian.Uint16(rect[4:6])), int(binary.BigEndian.Uint16(rect[6:8]))
				if binary.BigEndian.Uint32(rect[8:]) != 0 || x+w > r.width || y+h > r.height {
					return nil, errors.New("AMT RFB rectangle invalid")
				}
				pixels, err := readN(r.conn, w*h*2)
				if err != nil {
					return nil, err
				}
				for row := 0; row < h; row++ {
					copy(frame[((y+row)*r.width+x)*2:], pixels[row*w*2:(row+1)*w*2])
				}
			}
			img := image.NewRGBA(image.Rect(0, 0, r.width, r.height))
			for y := 0; y < r.height; y++ {
				for x := 0; x < r.width; x++ {
					v := binary.LittleEndian.Uint16(frame[(y*r.width+x)*2:])
					img.Set(x, y, color.RGBA{R: uint8((v >> 11 & 31) * 255 / 31), G: uint8((v >> 5 & 63) * 255 / 63), B: uint8((v & 31) * 255 / 31), A: 255})
				}
			}
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				return nil, errors.New("AMT PNG encoding failed")
			}
			return buf.Bytes(), nil
		case 1:
			header, err := readN(r.conn, 5)
			if err != nil {
				return nil, err
			}
			count := int(binary.BigEndian.Uint16(header[3:]))
			if count > 256 {
				return nil, errors.New("AMT RFB color map invalid")
			}
			if _, err = readN(r.conn, count*6); err != nil {
				return nil, err
			}
		case 2:
			continue
		case 3:
			header, err := readN(r.conn, 7)
			if err != nil {
				return nil, err
			}
			n := binary.BigEndian.Uint32(header[3:])
			if n > 4096 {
				return nil, errors.New("AMT RFB message too large")
			}
			if _, err = readN(r.conn, int(n)); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("AMT RFB message unsupported")
		}
	}
	return nil, errors.New("AMT RFB frame unavailable")
}
