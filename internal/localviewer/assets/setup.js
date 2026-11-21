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

  load();
  setInterval(load, 5000);
})();
