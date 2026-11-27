package localviewer

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestFilePermissionLabelDescribesWrites(t *testing.T) {
	for _, name := range []string{"assets/setup.html", "assets/index.html"} {
		data, err := assets.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "Allow agents to read, create and overwrite files in") {
			t.Errorf("%s: file permission label does not describe writes", name)
		}
	}
}

// Execute both complete page scripts with synthetic DOM and API fixtures, not
// an extracted replacement renderer. No browser, worker or node is started.
func TestPermissionCopyTracksCurrentSettings(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable")
	}
	setup, err := assets.ReadFile("assets/setup.js")
	if err != nil {
		t.Fatal(err)
	}
	permissions, err := assets.ReadFile("assets/permissions.js")
	if err != nil {
		t.Fatal(err)
	}
	script := `const assert = require("node:assert/strict");
const vm = require("node:vm");
const elements = new Map();
const element = id => {
  if (!elements.has(id)) elements.set(id, {
    textContent: "", hidden: true, checked: false, disabled: false, dataset: {},
    appendChild() {}, replaceChildren() {}, removeAttribute() {},
    set innerHTML(v) { throw Error("unexpected HTML rendering"); }
  });
  return elements.get(id);
};
const timers = new Map();
let state = {available: true, public_only: true, files: false, shell: false,
  console: false, desktop: false, workspace: "C:\\workspace\\<example>"};
let refuse = false;
const writes = [];
const staleEnroll = {stage: "connected", posture: "Agents can't open files", worker_offer: true};
const context = vm.createContext({
  document: {getElementById: element, createElement: () => element("child")},
  sessionStorage: {getItem: () => "fake-token"},
  window: {confirm: () => true},
  setInterval: (fn, ms) => { timers.set(ms, fn); return ms; }, clearInterval() {},
  fetch: async (path, options = {}) => {
    let data;
    let ok = true;
    if (path === "/api/node-permissions") {
      if (options.method === "POST") {
        const body = JSON.parse(options.body);
        writes.push(body);
        ok = !refuse;
        if (ok) state[body.surface] = body.enabled;
      }
      data = ok ? {...state} : {error: "Synthetic refusal"};
    } else if (path === "/api/setup/state") {
      data = {enroll_available: true, max_sessions: 1};
    } else if (path === "/api/setup/enroll/state") {
      data = staleEnroll;
    } else if (path === "/api/setup/posture/retry") {
      data = {message: "Synthetic refresh"};
    } else throw Error("unexpected request: " + path);
    return {ok, json: async () => data};
  }
});
const settle = async () => { for (let i = 0; i < 6; i++) await new Promise(setImmediate); };
const summary = () => element("node-perms-posture").textContent;
const assertShared = () => assert.equal(element("enroll-posture").textContent, summary());
(async () => {
  vm.runInContext(` + strconv.Quote(string(setup)) + `, context);
  // Enrollment may finish before the first permission read. Do not claim
  // specific capabilities from its initial receipt while that read is pending.
  await settle();
  assert.doesNotMatch(element("enroll-posture").textContent, /can't open files/);
  vm.runInContext(` + strconv.Quote(string(permissions)) + `, context);
  await settle();
  assertShared();
  assert.match(summary(), /can't open files/);
  assert.equal(writes.length, 0, "loading copy must not change permissions");
  element("perm-files").checked = true;
  await element("perm-files").onchange();
  assertShared();
  assert.match(summary(), /read, create and overwrite files/);
  assert.match(summary(), /C:\\workspace\\<example>/);
  assert.doesNotMatch(summary(), /can't open files/);
  assert.deepEqual(writes, [{confirm: true, surface: "files", enabled: true}]);
  // Enrollment still returns its old initial posture. It must not replace
  // the current permission summary after a later enrollment refresh.
  await element("posture-retry").onclick();
  assertShared();
  assert.match(summary(), /read, create and overwrite files/);
  refuse = true;
  element("perm-files").checked = false;
  await element("perm-files").onchange();
  assert.equal(element("perm-files").checked, true);
  assertShared();
  assert.match(summary(), /read, create and overwrite files/);
  assert.equal(element("node-perms-message").textContent, "Synthetic refusal");
  refuse = false;
  // Exercise every combination, including permission changes from elsewhere
  // observed by the ordinary GET poll. Console/desktop are display-only here.
  for (let mask = 0; mask < 16; mask++) {
    ["files", "shell", "console", "desktop"].forEach((key, i) => state[key] = !!(mask & (1 << i)));
    await timers.get(10000)();
    assertShared();
    assert.equal(element("perm-files").checked, state.files);
    assert.equal(element("perm-shell").checked, state.shell);
    assert.equal(summary().includes("read, create and overwrite files"), state.files);
    assert.equal(summary().includes("can't open files"), !state.files);
    assert.equal(summary().includes("can't run commands"), !state.shell);
    assert.equal(summary().includes("can't use a terminal"), !state.console);
    assert.equal(summary().includes("can't see or control the screen"), !state.desktop);
    assert.match(summary(), /public websites only/);
  }
  state.public_only = false;
  await timers.get(10000)();
  assert.doesNotMatch(summary(), /public websites only/);
  state.available = false;
  await timers.get(10000)();
  assert.equal(element("node-perms").hidden, true);
  assert.equal(summary(), "");
  assert.doesNotMatch(element("enroll-posture").textContent, /can't open files|read, create and overwrite/);
  assert.equal(writes.length, 2, "display refresh must never submit a permission change");
})().catch(e => { console.error(e); process.exitCode = 1; });`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("permission copy: %v\n%s", err, out)
	}
}
