// Citadel: this PC's AceTeam permissions (Files and Shell), shared by the setup
// page and the viewer. Plain JavaScript, no build step. Every value from the
// server is rendered with textContent, never as HTML. Only a click here changes
// a permission; turning commands on asks a second time.
"use strict";

(function () {
  const $ = (id) => document.getElementById(id);
  const box = $("node-perms");
  if (!box) return;
  const csrf = sessionStorage.getItem("citadelViewerToken") || "";
  const headers = { "Content-Type": "application/json", "X-Citadel-CSRF": csrf };
  let busy = false;

  function renderPosture(text) {
    $("node-perms-posture").textContent = text;
    // The enrollment receipt describes the initial defaults, not later human
    // choices. Keep its visible summary on the same current settings as below.
    const enrollPosture = $("enroll-posture");
    if (enrollPosture) enrollPosture.textContent = text || "Check the current permission settings below before allowing agents to use this PC.";
  }

  function render(st) {
    box.hidden = !st.available;
    if (!st.available) { renderPosture(""); return; }
    $("perm-files").checked = !!st.files;
    $("perm-shell").checked = !!st.shell;
    $("perm-workspace").textContent = st.workspace || "the Citadel workspace folder";
    const parts = [];
    if (st.public_only) parts.push("Co-browse reaches public websites only, not your home network.");
    parts.push(st.files ?
      "Agents may read, create and overwrite files in " + (st.workspace || "the Citadel workspace folder") + "." :
      "Agents can't open files or create or overwrite files in the workspace.");
    parts.push(st.shell ? "Command access is on for this PC." : "Agents can't run commands on this PC.");
    parts.push(st.console ? "Remote terminal (console) access is on for this PC." : "Agents can't use a terminal on this PC.");
    parts.push(st.desktop ? "Remote screen (desktop) access is on for this PC." : "Agents can't see or control the screen on this PC.");
    renderPosture(parts.join(" "));
    $("node-perms-message").textContent = st.message || "";
  }

  async function load() {
    if (busy) return;
    try {
      const r = await fetch("/api/node-permissions", { credentials: "same-origin", headers: { "X-Citadel-CSRF": csrf } });
      if (r.ok) render(await r.json());
    } catch (e) { /* the page shows its own connection state */ }
  }

  async function change(surface, input) {
    const enabled = input.checked;
    const body = { confirm: true, surface, enabled };
    if (surface === "shell" && enabled) {
      const ok = window.confirm("Allow AceTeam agents to run commands on this PC?\n\n" +
        "Commands run with the Citadel worker's permissions and can read, change or delete anything it can. " +
        "Allow this only for a task that needs it, and turn it off afterwards.");
      if (!ok) { input.checked = false; return; }
      body.confirm_shell = true;
    }
    busy = true;
    input.disabled = true;
    try {
      const r = await fetch("/api/node-permissions", { method: "POST", credentials: "same-origin", headers, body: JSON.stringify(body) });
      let data = {};
      try { data = await r.json(); } catch (e) { /* not JSON */ }
      if (!r.ok) {
        input.checked = !enabled;
        $("node-perms-message").textContent = data.error || r.statusText;
      } else {
        render(data);
      }
    } catch (e) {
      input.checked = !enabled;
    }
    input.disabled = false;
    busy = false;
  }

  $("perm-files").onchange = () => change("files", $("perm-files"));
  $("perm-shell").onchange = () => change("shell", $("perm-shell"));
  load();
  setInterval(load, 10000);
})();
