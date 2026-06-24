package session

import "testing"

func TestEvaluateLinux(t *testing.T) {
	tests := []struct {
		name        string
		env         linuxEnv
		wantDesktop bool
		wantType    string
	}{
		{
			name:        "x11 display set",
			env:         linuxEnv{display: ":0"},
			wantDesktop: true,
			wantType:    "x11",
		},
		{
			name:        "non-zero display (issue #287)",
			env:         linuxEnv{display: ":1"},
			wantDesktop: true,
			wantType:    "x11",
		},
		{
			name:        "wayland takes precedence",
			env:         linuxEnv{display: ":0", waylandDisplay: "wayland-0"},
			wantDesktop: true,
			wantType:    "wayland",
		},
		{
			name:        "headless: no display vars",
			env:         linuxEnv{},
			wantDesktop: false,
			wantType:    "headless",
		},
		{
			name:        "headless reports xdg session type when known",
			env:         linuxEnv{xdgSessionType: "tty"},
			wantDesktop: false,
			wantType:    "tty",
		},
		{
			name:        "unspecified xdg type falls back to headless",
			env:         linuxEnv{xdgSessionType: "unspecified"},
			wantDesktop: false,
			wantType:    "headless",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateLinux(tt.env)
			if got.HasDesktop != tt.wantDesktop {
				t.Errorf("HasDesktop = %v, want %v", got.HasDesktop, tt.wantDesktop)
			}
			if got.SessionType != tt.wantType {
				t.Errorf("SessionType = %q, want %q", got.SessionType, tt.wantType)
			}
			if got.Reason == "" {
				t.Error("Reason should never be empty")
			}
		})
	}
}

func TestEvaluateDarwin(t *testing.T) {
	tests := []struct {
		name        string
		env         darwinEnv
		consoleUser string
		wantDesktop bool
		wantType    string
	}{
		{
			name:        "aqua gui session",
			env:         darwinEnv{},
			consoleUser: "jason",
			wantDesktop: true,
			wantType:    "aqua",
		},
		{
			name:        "ssh session overrides console user",
			env:         darwinEnv{sshTTY: "/dev/ttys000"},
			consoleUser: "jason",
			wantDesktop: false,
			wantType:    "ssh",
		},
		{
			name:        "ssh connection env",
			env:         darwinEnv{sshConnection: "1.2.3.4 22 5.6.7.8 22"},
			consoleUser: "jason",
			wantDesktop: false,
			wantType:    "ssh",
		},
		{
			name:        "no console user is headless",
			env:         darwinEnv{},
			consoleUser: "",
			wantDesktop: false,
			wantType:    "headless",
		},
		{
			name:        "loginwindow means no one logged in",
			env:         darwinEnv{},
			consoleUser: "loginwindow",
			wantDesktop: false,
			wantType:    "headless",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateDarwin(tt.env, tt.consoleUser)
			if got.HasDesktop != tt.wantDesktop {
				t.Errorf("HasDesktop = %v, want %v", got.HasDesktop, tt.wantDesktop)
			}
			if got.SessionType != tt.wantType {
				t.Errorf("SessionType = %q, want %q", got.SessionType, tt.wantType)
			}
			if got.Reason == "" {
				t.Error("Reason should never be empty")
			}
		})
	}
}

func TestEvaluateWindows(t *testing.T) {
	const noConsole = uint32(0xFFFFFFFF)
	tests := []struct {
		name           string
		procSession    uint32
		consoleSession uint32
		wantDesktop    bool
		wantType       string
	}{
		{
			name:           "interactive console session",
			procSession:    1,
			consoleSession: 1,
			wantDesktop:    true,
			wantType:       "console",
		},
		{
			name:           "session 0 isolation",
			procSession:    0,
			consoleSession: 1,
			wantDesktop:    false,
			wantType:       "session0",
		},
		{
			name:           "no active console attached",
			procSession:    2,
			consoleSession: noConsole,
			wantDesktop:    false,
			wantType:       "headless",
		},
		{
			name:           "process in different session than console",
			procSession:    3,
			consoleSession: 1,
			wantDesktop:    false,
			wantType:       "disconnected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateWindows(tt.procSession, tt.consoleSession)
			if got.HasDesktop != tt.wantDesktop {
				t.Errorf("HasDesktop = %v, want %v", got.HasDesktop, tt.wantDesktop)
			}
			if got.SessionType != tt.wantType {
				t.Errorf("SessionType = %q, want %q", got.SessionType, tt.wantType)
			}
			if got.Reason == "" {
				t.Error("Reason should never be empty")
			}
		})
	}
}

func TestApplyDerived(t *testing.T) {
	t.Run("desktop available enables vnc/screenshot/input", func(t *testing.T) {
		d := &DesktopInfo{HasDesktop: true}
		d.applyDerived()
		if !d.VNC || !d.Screenshot || !d.Input {
			t.Errorf("expected vnc/screenshot/input all true, got vnc=%v screenshot=%v input=%v",
				d.VNC, d.Screenshot, d.Input)
		}
		if !d.Terminal {
			t.Error("terminal should always be true")
		}
	})
	t.Run("headless disables vnc/screenshot/input but keeps terminal", func(t *testing.T) {
		d := &DesktopInfo{HasDesktop: false}
		d.applyDerived()
		if d.VNC || d.Screenshot || d.Input {
			t.Errorf("expected vnc/screenshot/input all false, got vnc=%v screenshot=%v input=%v",
				d.VNC, d.Screenshot, d.Input)
		}
		if !d.Terminal {
			t.Error("terminal should always be true even when headless")
		}
	})
}

func TestCapabilityMap(t *testing.T) {
	d := &DesktopInfo{HasDesktop: false}
	d.applyDerived()
	m := d.CapabilityMap()

	wantKeys := []string{"desktop", "vnc", "screenshot", "input_injection", "terminal"}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("capability map missing key %q", k)
		}
	}
	if m["desktop"] {
		t.Error("headless node should report desktop=false")
	}
	if m["vnc"] || m["screenshot"] || m["input_injection"] {
		t.Error("headless node should report vnc/screenshot/input_injection=false")
	}
	if !m["terminal"] {
		t.Error("terminal should be true")
	}
}

func TestDetectDesktopNeverNil(t *testing.T) {
	got := DetectDesktop()
	if got == nil {
		t.Fatal("DetectDesktop must never return nil")
	}
	if got.Reason == "" {
		t.Error("DetectDesktop must always set a Reason")
	}
	// Terminal is platform-independent and always advertised.
	if !got.Terminal {
		t.Error("Terminal capability should always be advertised")
	}
}

func TestParseConsoleUser(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{
			name: "standard dictionary with name",
			input: "<dictionary> {\n" +
				"  GID : 20\n" +
				"  Name : jason\n" +
				"  UID : 501\n" +
				"}\n",
			expect: "jason",
		},
		{
			name:   "loginwindow at login screen",
			input:  "<dictionary> {\n  Name : loginwindow\n}\n",
			expect: "loginwindow",
		},
		{
			name:   "no name key",
			input:  "<dictionary> {\n  GID : 20\n}\n",
			expect: "",
		},
		{
			name:   "empty output",
			input:  "",
			expect: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseConsoleUser(tt.input); got != tt.expect {
				t.Errorf("parseConsoleUser() = %q, want %q", got, tt.expect)
			}
		})
	}
}
