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

  function render(st) {
    box.hidden = !st.available;
    if (!st.available) return;
    $("perm-files").checked = !!st.files;
    $("perm-shell").checked = !!st.shell;
    $("perm-workspace").textContent = st.workspace || "the Citadel workspace folder";
    const parts = [];
    if (st.public_only) parts.push("Co-browse reaches public websites only, not your home network.");
    if (!st.files && !st.shell) parts.push("Agents can't open files or run commands on this PC.");
    $("node-perms-posture").textContent = parts.join(" ");
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
