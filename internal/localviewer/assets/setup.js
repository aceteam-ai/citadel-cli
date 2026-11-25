// Citadel local setup page. Plain JavaScript, no build step. Every value from
// the server is rendered with textContent, never as HTML.
"use strict";

(function () {
  const csrf = sessionStorage.getItem("citadelViewerToken") || "";
  const $ = (id) => document.getElementById(id);
  const headers = { "Content-Type": "application/json", "X-Citadel-CSRF": csrf };

  async function api(method, path, body) {
    const r = await fetch(path, {
      method,
      credentials: "same-origin",
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    let data = {};
    try { data = await r.json(); } catch (e) { /* not JSON */ }
    if (!r.ok) throw new Error(data.error || r.statusText || "request failed");
    return data;
  }

  const LABEL = { ok: "Ready", note: "Note", FAIL: "Needs attention" };
  const TITLE = {
    "Browser": "Web browser", "Test launch": "Hidden browser test", "Viewer port": "Local viewer",
    "Passphrase": "Passphrase", "Agent apps": "Other agent apps", "Audit log": "Activity log",
    "Harness": "Agent app settings", "Network": "Privacy",
  };

  function li(parent, text, cls) {
    const e = document.createElement("li");
    e.textContent = text;
    if (cls) e.className = cls;
    parent.appendChild(e);
    return e;
  }

  function renderChecks(checks) {
    const ul = $("checks");
    ul.replaceChildren();
    for (const c of checks || []) {
      const e = li(ul, (LABEL[c.status] || c.status) + ": " + (TITLE[c.what] || c.what) + ". " + c.detail, c.status === "FAIL" ? "danger" : "");
      e.dataset.status = c.status;
    }
  }

  function renderApps(apps) {
    const ul = $("apps");
    ul.replaceChildren();
    for (const a of apps || []) {
      const row = document.createElement("li");
      const name = document.createElement("strong");
      name.textContent = a.title + " ";
      row.appendChild(name);
      const where = document.createElement("span");
      where.className = "muted";
      if (a.state === "not_found") {
        where.textContent = "not found on this computer";
        row.appendChild(where);
      } else {
        where.textContent = a.state === "connected" ? "connected (" + a.path + ")" : a.path;
        row.appendChild(where);
        if (a.state !== "connected") {
          const b = document.createElement("button");
          b.className = "primary";
          b.textContent = "Connect";
          b.onclick = () => connect(a, b);
          row.appendChild(b);
        }
      }
      if (a.note) {
        const n = document.createElement("div");
        n.className = "muted";
        n.textContent = "Note: " + a.note;
        row.appendChild(n);
      }
      ul.appendChild(row);
    }
  }

  async function connect(app, button) {
    button.disabled = true;
    try {
      const res = await api("POST", "/api/setup/connect/" + encodeURIComponent(app.name), { confirm: true, path: app.path });
      $("connect-result").textContent = res.message + " " + (res.next_step || "");
      await load();
    } catch (e) {
      $("connect-result").textContent = "Could not connect " + app.title + ": " + e.message;
      button.disabled = false;
    }
  }

  function renderSessions(st) {
    $("limit").textContent = "Up to " + st.max_sessions + " sessions at once.";
    const ul = $("sessions");
    ul.replaceChildren();
    for (const s of st.sessions || []) {
      li(ul, (s.goal || s.id) + (s.domain ? " on " + s.domain : "") + " (" + (s.driver === "human" ? "you are driving" : "agent driving") + ")");
    }
    if (!(st.sessions || []).length) li(ul, "No live sessions. Ask your agent to browse a site.");
    $("sessions-note").textContent = st.sessions_note || "";
  }

  async function load() {
    try {
      const st = await api("GET", "/api/setup/state");
      renderChecks(st.checks);
      renderApps(st.apps);
      renderSessions(st);
      $("generic").value = st.generic_entry || "";
    } catch (e) {
      $("sessions-note").textContent = "This setup tab is no longer connected. Run citadel local setup to open a new one.";
    }
  }

  $("full-check").onclick = async () => {
    $("full-check").disabled = true;
    try { renderChecks((await api("POST", "/api/setup/doctor", {})).checks); } catch (e) { /* shown by load */ }
    $("full-check").disabled = false;
  };
  $("copy").onclick = async () => {
    $("generic").select();
    try { await navigator.clipboard.writeText($("generic").value); } catch (e) { document.execCommand("copy"); }
  };
  $("open-viewer").onclick = async () => {
    try { await api("POST", "/api/setup/open-viewer", {}); $("viewer-result").textContent = "Opened the viewer in a new tab."; }
    catch (e) { $("viewer-result").textContent = e.message; }
  };

  // Connect this PC to AceTeam. The server keeps the device code; the page
  // only ever sees the user code, the approval link and fixed messages.
  const ACTIVE = { starting: true, waiting: true, connecting: true, canceling: true };
  let enrollTimer = null;
  let qrFor = "";
  let qrURL = "";

  function clearQR() {
    if (qrURL) URL.revokeObjectURL(qrURL);
    qrURL = "";
    qrFor = "";
    $("enroll-qr").hidden = true;
    $("enroll-qr").removeAttribute("src");
  }

  async function showQR(link) {
    if (qrFor === link) return;
    clearQR();
    qrFor = link;
    try {
      const r = await fetch("/api/setup/enroll/qr", { credentials: "same-origin", headers: { "X-Citadel-CSRF": csrf } });
      if (!r.ok || qrFor !== link) return;
      qrURL = URL.createObjectURL(await r.blob());
      $("enroll-qr").src = qrURL;
      $("enroll-qr").hidden = false;
    } catch (e) { /* the link and code are enough */ }
  }

  function renderEnroll(st) {
    const active = !!ACTIVE[st.stage];
    // After "already" the button stays, for a try after `citadel logout`.
    $("enroll-start").hidden = active || st.stage === "connected";
    $("enroll-start").disabled = active;
    $("enroll-cancel").hidden = !active || st.stage === "canceling";
    $("enroll-message").textContent = st.message || "";
    $("enroll-message").className = st.stage === "failed" ? "refused" : "";
    const waiting = st.stage === "waiting" && st.user_code && st.verification_url;
    $("enroll-code").hidden = !waiting;
    if (waiting) {
      $("enroll-user-code").textContent = st.user_code;
      $("enroll-link").href = st.verification_url;
      showQR(st.verification_url);
    } else {
      clearQR();
    }
    const done = $("enroll-done");
    done.hidden = !(st.stage === "connected" || st.stage === "already");
    if (!done.hidden) {
      const node = st.node_name ? "\"" + st.node_name + "\"" : "This PC";
      const org = st.org_name ? " in " + st.org_name : "";
      done.textContent = st.stage === "connected" ?
        "This PC is now " + node + org + "." :
        node + " is already connected" + org + ".";
    }
    $("enroll-posture").hidden = !(st.stage === "connected" && st.posture);
    $("enroll-posture").textContent = st.posture || "";
    $("worker").hidden = !(st.stage === "connected" && st.worker_offer);
    $("posture-retry-row").hidden = !(st.stage === "connected" && st.posture_retry);
    if (active && !enrollTimer) enrollTimer = setInterval(pollEnroll, 1500);
    if (!active && enrollTimer) { clearInterval(enrollTimer); enrollTimer = null; }
  }

  async function pollEnroll() {
    try { renderEnroll(await api("GET", "/api/setup/enroll/state")); } catch (e) { /* shown by load */ }
  }

  $("enroll-start").onclick = async () => {
    $("enroll-start").disabled = true;
    try { renderEnroll(await api("POST", "/api/setup/enroll/start", { confirm: true })); }
    catch (e) { $("enroll-message").textContent = e.message; await pollEnroll(); }
  };
  $("enroll-cancel").onclick = async () => {
    try { renderEnroll(await api("POST", "/api/setup/enroll/cancel", { confirm: true })); } catch (e) { /* next poll */ }
  };

  const STEP = { ok: "Done", failed: "Failed", skipped: "Skipped", not_applicable: "Not needed" };

  function renderSteps(listId, msgId, res) {
    const ul = $(listId);
    ul.replaceChildren();
    for (const s of res.steps || []) {
      li(ul, s.name + ": " + (STEP[s.status] || s.status) + ". " + s.message, s.status === "failed" ? "danger" : "");
    }
    $(msgId).textContent = res.message || "";
    $(msgId).className = res.ok ? "" : "refused";
  }

  async function runAction(path, button, listId, msgId) {
    button.disabled = true;
    $(msgId).textContent = "Working...";
    $(msgId).className = "";
    try {
      renderSteps(listId, msgId, await api("POST", path, { confirm: true }));
    } catch (e) {
      $(listId).replaceChildren();
      $(msgId).textContent = e.message;
      $(msgId).className = "refused";
    }
    button.disabled = false;
  }

  $("posture-retry").onclick = async () => {
    $("posture-retry").disabled = true;
    try {
      const res = await api("POST", "/api/setup/posture/retry", { confirm: true });
      $("posture-retry-message").textContent = res.message || "";
    } catch (e) { $("posture-retry-message").textContent = e.message; }
    $("posture-retry").disabled = false;
    await pollEnroll();
  };
  $("worker-start").onclick = () => runAction("/api/setup/worker/start", $("worker-start"), "worker-steps", "worker-message");
  $("disconnect-start").onclick = async () => {
    const ok = window.confirm("Disconnect this PC from AceTeam?\n\nThe background worker stops, Citadel no longer starts when you sign in, " +
      "this PC is logged out of AceTeam and its AceTeam device key is removed. Local mode keeps working, and your saved logins are not touched.");
    if (!ok) return;
    await runAction("/api/setup/disconnect", $("disconnect-start"), "disconnect-steps", "disconnect-message");
    await pollEnroll();
  };

  async function loadEnroll() {
    try {
      const st = await api("GET", "/api/setup/state");
      $("disconnect").hidden = !st.disconnect_available;
      if (!st.enroll_available) return;
      $("enroll").hidden = false;
      await pollEnroll();
    } catch (e) { /* shown by load */ }
  }

  load();
  loadEnroll();
  setInterval(load, 5000);
})();
