// Citadel local viewer. Plain JavaScript, no build step. Every value from the
// server is rendered with textContent, never as HTML.
"use strict";

(function () {
  const csrf = sessionStorage.getItem("citadelViewerToken") || "";
  const $ = (id) => document.getElementById(id);
  let state = { agent: "", sessions: [] };
  let selected = null;
  let ws = null;
  let wsFor = null;
  let frameURL = null;
  let human = false;

  // post sends one viewer action. It reports a failure in msgEl (default: the
  // hint line) and resolves to true on success.
  async function post(id, action, body, msgEl) {
    const r = await fetch("/api/sessions/" + encodeURIComponent(id) + "/" + action, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-Citadel-CSRF": csrf },
      body: JSON.stringify(body || {}),
    });
    if (!r.ok) {
      let msg = r.statusText;
      try { msg = (await r.json()).error || msg; } catch (e) { /* not JSON */ }
      (msgEl || $("hint")).textContent = msg;
    }
    poll();
    return r.ok;
  }

  async function poll() {
    try {
      const r = await fetch("/api/state", { credentials: "same-origin", headers: { "X-Citadel-CSRF": csrf } });
      if (!r.ok) {
        $("agent-line").textContent = "This viewer tab is no longer connected. Run citadel local viewer to open a new one.";
        return;
      }
      state = await r.json();
      render();
    } catch (e) {
      $("agent-line").textContent = "Citadel is not running (the agent app may have closed).";
    }
  }

  function domainOf(url) {
    try { return new URL(url).host || url; } catch (e) { return url || ""; }
  }

  function render() {
    $("agent-line").textContent = state.agent ? "Connected agent: " + state.agent : "No agent connected";
    const sessions = state.sessions || [];
    const tabs = $("tabs");
    tabs.replaceChildren();
    if (!sessions.find((s) => s.session_id === selected)) {
      selected = sessions.length ? sessions[0].session_id : null;
    }
    for (const s of sessions) {
      const b = document.createElement("button");
      b.textContent = (s.goal || s.session_id) + (s.state === "locked" ? " (locked)" : (s.driver === "human" ? " (you)" : ""));
      if (s.session_id === selected) b.className = "sel";
      b.onclick = () => { selected = s.session_id; render(); };
      tabs.appendChild(b);
    }
    const waitingAny = sessions.some((s) => (s.driver === "human" && s.handoff_reason) || s.approval_asked || s.commit || s.state === "locked");
    document.title = waitingAny ? "● Citadel: waiting for you" : "Citadel viewer";

    const s = sessions.find((x) => x.session_id === selected);
    $("empty").hidden = !!s;
    $("session").hidden = !s;
    if (!s) { closeStream(); return; }

    const locked = s.state === "locked";
    $("unlock").hidden = !locked;
    $("controls").hidden = locked;
    $("screen-wrap").hidden = locked;
    if (locked) {
      renderUnlock(s);
      $("session-goal").textContent = s.goal || "Browser session";
      $("session-driver").textContent = "Locked";
      $("session-driver").className = "pill";
      $("session-domain").textContent = domainOf(s.url);
      $("session-grant").textContent = "";
      $("waiting").hidden = true;
      $("approval").hidden = true;
      $("commit").hidden = true;
      $("commit-result").hidden = true;
      $("hint").textContent = "";
      closeStream();
      return;
    }

    human = s.driver === "human";
    $("session-goal").textContent = s.goal || "Browser session";
    $("session-driver").textContent = human ? "You are driving" : "Agent driving";
    $("session-driver").className = human ? "pill human" : "pill";
    $("session-domain").textContent = domainOf(s.url);
    $("session-grant").textContent = s.approval === "approved"
      ? "AI actions allowed on " + (s.domain || "this site") + " until " + new Date(s.grant_expires_at).toLocaleTimeString()
      : (s.approval === "denied" ? "AI actions denied" : (s.approval === "revoked" ? "AI actions revoked" : "AI actions not approved"));

    const w = $("waiting");
    if (human && s.handoff_reason && s.handoff_reason !== "taken by the user") {
      w.hidden = false;
      w.textContent = "Waiting for you: " + s.handoff_reason + ". Then press Hand back to agent.";
    } else if (human) {
      w.hidden = false;
      w.textContent = "You are driving. The agent cannot see or act until you hand back.";
    } else {
      w.hidden = true;
    }

    // The site the agent asked about, which may differ from the page shown.
    const askedSite = s.asked_domain || s.domain || domainOf(s.url);
    const showCard = s.approval_asked && !s.commit && !(s.grants || []).some((g) => g.domain === askedSite);
    $("approval").hidden = !showCard;
    $("approval-site").textContent = askedSite;
    $("approval-domain").textContent = "Session: " + (s.goal || s.session_id);
    $("revoke").hidden = !(s.grants && s.grants.length);
    renderCommit(s);

    $("drive").textContent = human ? "Hand back to agent" : "Take control";
    const addr = $("address");
    addr.disabled = !human;
    if (document.activeElement !== addr) addr.value = s.url || "";
    $("screen-wrap").className = human ? "driving" : "";
    $("hint").textContent = human ? "Click the picture and type; your keystrokes go only to this browser." : "";
    openStream(s.session_id);
  }

  function renderUnlock(s) {
    const vault = state.profile_vault;
    $("unlock-profile").textContent = s.profile ? "(profile: " + s.profile + ")" : "";
    const busy = !!s.unlocking;
    $("unlock-busy").hidden = !busy;
    $("unlock-enter").hidden = busy || !vault || !vault.configured || !vault.strong;
    $("unlock-setup").hidden = busy || !vault || vault.configured;
    $("unlock-weak").hidden = busy || !vault || !vault.configured || vault.strong;
    if (state.profile_vault_error) $("unlock-error").textContent = state.profile_vault_error;
    $("unlock-throwaway").disabled = busy;
  }

  let commitShown = null;
  function clearShot() {
    const cv = $("commit-shot");
    cv.hidden = true;
    cv.getContext("2d").clearRect(0, 0, cv.width, cv.height);
  }
  async function renderCommit(s) {
    const c = s.commit;
    const r = s.commit_result;
    $("commit").hidden = !c;
    $("commit-result").hidden = !!c || !r;
    if (!c && r) {
      $("commit-result").textContent = r.state === "executed"
        ? "Approved and done: " + r.action + " on " + r.domain + "."
        : (r.state === "changed" ? "The page changed, so " + r.action + " on " + r.domain + " was not done. The agent will ask again."
          : "Not done (" + r.state + "): " + r.action + " on " + r.domain + ". " + (r.detail || ""));
    }
    if (!c) { commitShown = null; clearShot(); return; }
    $("commit-action").textContent = c.action;
    $("commit-domain").textContent = c.domain;
    $("commit-reason").textContent = "Why you are asked: " + c.reason + ".";
    $("commit-approve").hidden = !!c.unverifiable;
    if (c.unverifiable) $("commit-reason").textContent += " Citadel cannot verify this one, so it cannot be done for you: do it yourself in the browser, or deny it.";
    if (commitShown === c.id || !c.has_shot) return;
    // A different request: whatever was drawn belongs to the old one.
    clearShot();
    commitShown = c.id;
    const mine = c.id;
    try {
      const r = await fetch("/api/sessions/" + encodeURIComponent(s.session_id) + "/commit/" + encodeURIComponent(c.id) + "/shot",
        { credentials: "same-origin", headers: { "X-Citadel-CSRF": csrf } });
      if (!r.ok || commitShown !== mine) return;
      const body = await r.json();
      if (commitShown !== mine) return;
      const bin = atob(body.png);
      const bytes = new Uint8Array(bin.length);
      for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      const url = URL.createObjectURL(new Blob([bytes], { type: "image/png" }));
      const img = new Image();
      img.onload = () => {
        URL.revokeObjectURL(url);
        if (commitShown !== mine) return; // a late load must not draw an old crop
        const cv = $("commit-shot");
        const rect = body.rect || {};
        const scale = rect.vw ? img.naturalWidth / rect.vw : 1;
        let sx = 0, sy = 0, sw = img.naturalWidth, sh = Math.min(img.naturalHeight, img.naturalWidth / 2);
        if (rect.w > 0 && rect.h > 0) {
          const padX = Math.max(80, rect.w), padY = Math.max(60, rect.h);
          sx = Math.max(0, (rect.x - padX) * scale);
          sy = Math.max(0, (rect.y - padY) * scale);
          sw = Math.min(img.naturalWidth - sx, (rect.w + 2 * padX) * scale);
          sh = Math.min(img.naturalHeight - sy, (rect.h + 2 * padY) * scale);
        }
        cv.width = 480; cv.height = Math.max(60, Math.round(480 * sh / Math.max(1, sw)));
        const g = cv.getContext("2d");
        g.drawImage(img, sx, sy, sw, sh, 0, 0, cv.width, cv.height);
        if (rect.w > 0 && rect.h > 0) {
          const k = cv.width / sw;
          g.strokeStyle = "#d62424"; g.lineWidth = 3;
          g.strokeRect((rect.x * scale - sx) * k, (rect.y * scale - sy) * k, rect.w * scale * k, rect.h * scale * k);
        }
        cv.hidden = false;
      };
      img.src = url;
    } catch (e) { /* the card still names the action */ }
  }

  function closeStream() {
    if (ws) { ws.onclose = null; ws.close(); }
    ws = null; wsFor = null;
  }

  function openStream(id) {
    if (wsFor === id && ws && ws.readyState <= 1) return;
    closeStream();
    wsFor = id;
    ws = new WebSocket("ws://" + location.host + "/api/stream?id=" + encodeURIComponent(id) + "&csrf=" + encodeURIComponent(csrf));
    ws.binaryType = "blob";
    ws.onmessage = (ev) => {
      if (typeof ev.data === "string") return; // init / select replies
      const url = URL.createObjectURL(ev.data);
      $("screen").src = url;
      if (frameURL) URL.revokeObjectURL(frameURL);
      frameURL = url;
    };
    ws.onclose = () => { ws = null; setTimeout(() => { wsFor = null; }, 1500); };
  }

  function send(msg) {
    if (human && ws && ws.readyState === 1) ws.send(JSON.stringify(msg));
  }

  function pos(ev) {
    const r = $("screen").getBoundingClientRect();
    return { x: (ev.clientX - r.left) / r.width, y: (ev.clientY - r.top) / r.height };
  }

  const buttonName = (b) => ["left", "middle", "right", "back", "forward"][b] || "none";
  function modifiers(ev) {
    return (ev.altKey ? 1 : 0) | (ev.ctrlKey ? 2 : 0) | (ev.metaKey ? 4 : 0) | (ev.shiftKey ? 8 : 0);
  }

  const screen = $("screen");
  let lastMove = 0;
  screen.addEventListener("mousedown", (ev) => {
    if (!human) return;
    ev.preventDefault();
    $("screen-wrap").focus();
    const p = pos(ev);
    send({ type: "mouse", event: "mousePressed", x: p.x, y: p.y, button: buttonName(ev.button), buttons: ev.buttons, clickCount: ev.detail || 1 });
  });
  screen.addEventListener("mouseup", (ev) => {
    if (!human) return;
    const p = pos(ev);
    send({ type: "mouse", event: "mouseReleased", x: p.x, y: p.y, button: buttonName(ev.button), buttons: ev.buttons, clickCount: ev.detail || 1 });
  });
  screen.addEventListener("mousemove", (ev) => {
    const now = Date.now();
    if (!human || now - lastMove < 40) return;
    lastMove = now;
    const p = pos(ev);
    send({ type: "mouse", event: "mouseMoved", x: p.x, y: p.y, button: "none", buttons: ev.buttons });
  });
  screen.addEventListener("wheel", (ev) => {
    if (!human) return;
    ev.preventDefault();
    const p = pos(ev);
    send({ type: "mouse", event: "mouseWheel", x: p.x, y: p.y, deltaX: ev.deltaX, deltaY: ev.deltaY });
  }, { passive: false });
  screen.addEventListener("contextmenu", (ev) => { if (human) ev.preventDefault(); });

  const wrap = $("screen-wrap");
  wrap.addEventListener("keydown", (ev) => {
    if (!human) return;
    if ((ev.ctrlKey || ev.metaKey) && ev.key.toLowerCase() === "v") return; // paste event below
    ev.preventDefault();
    const printable = ev.key.length === 1 && !ev.ctrlKey && !ev.metaKey;
    send({ type: "key", keyEvent: printable ? "keyDown" : "rawKeyDown", key: ev.key, code: ev.code,
      text: printable ? ev.key : "", keyCode: ev.keyCode, modifiers: modifiers(ev) });
  });
  wrap.addEventListener("keyup", (ev) => {
    if (!human) return;
    ev.preventDefault();
    send({ type: "key", keyEvent: "keyUp", key: ev.key, code: ev.code, keyCode: ev.keyCode, modifiers: modifiers(ev) });
  });
  wrap.addEventListener("paste", (ev) => {
    if (!human) return;
    ev.preventDefault();
    const text = (ev.clipboardData && ev.clipboardData.getData("text/plain")) || "";
    if (text) send({ type: "paste", text: text.slice(0, 4096) });
  });

  $("drive").onclick = () => selected && post(selected, human ? "hand-back" : "take-control");
  $("approve").onclick = () => {
    const s = (state.sessions || []).find((x) => x.session_id === selected);
    if (selected && s) post(selected, "approve", { hours: Number($("approve-hours").value), domain: s.asked_domain || s.domain || "" });
  };
  $("commit-approve").onclick = () => {
    const s = (state.sessions || []).find((x) => x.session_id === selected);
    if (selected && s && s.commit) post(selected, "approve-commit", { commit: s.commit.id });
  };
  $("commit-deny").onclick = () => {
    const s = (state.sessions || []).find((x) => x.session_id === selected);
    if (selected && s && s.commit) post(selected, "deny-commit", { commit: s.commit.id });
  };
  $("deny").onclick = () => selected && post(selected, "deny");
  $("revoke").onclick = () => selected && post(selected, "revoke");
  $("stop").onclick = () => {
    if (!selected) return;
    const s = (state.sessions || []).find((x) => x.session_id === selected);
    const saved = s && s.profile_mode === "unlocked";
    const msg = saved ? "End this browser session? Its logins are encrypted and kept for next time."
      : "End this browser session? Its cookies are deleted.";
    if (confirm(msg)) post(selected, "stop");
  };

  // Passphrase fields are cleared as soon as they are sent; the value never
  // goes anywhere but the unlock request.
  async function unlock() {
    if (!selected) return;
    const pass = $("unlock-pass").value;
    $("unlock-pass").value = "";
    $("unlock-error").textContent = "";
    if (!pass) { $("unlock-error").textContent = "Enter your passphrase."; return; }
    await post(selected, "unlock", { passphrase: pass }, $("unlock-error"));
  }
  $("unlock-go").onclick = unlock;
  $("unlock-pass").addEventListener("keydown", (ev) => { if (ev.key === "Enter") unlock(); });
  $("setup-go").onclick = async () => {
    if (!selected) return;
    const pass = $("setup-pass").value, confirmPass = $("setup-confirm").value;
    $("setup-pass").value = ""; $("setup-confirm").value = "";
    $("unlock-error").textContent = "";
    if (!pass) { $("unlock-error").textContent = "Choose a passphrase."; return; }
    if (pass !== confirmPass) { $("unlock-error").textContent = "The two passphrases do not match."; return; }
    if (!$("setup-ack").checked) { $("unlock-error").textContent = "Tick the box to confirm you understand the passphrase cannot be recovered."; return; }
    await post(selected, "setup-passphrase", { passphrase: pass, confirm: confirmPass, ack: true }, $("unlock-error"));
  };
  $("unlock-throwaway").onclick = () => selected &&
    confirm("Start this session without saved logins? It starts logged out and nothing is saved.") &&
    post(selected, "use-throwaway", {}, $("unlock-error"));
  $("address").addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && selected && human) {
      let url = $("address").value.trim();
      if (url && !/^https?:\/\//i.test(url)) url = "https://" + url;
      post(selected, "navigate", { url });
    }
  });

  function hostPath(u) {
    try { const x = new URL(u); return x.host + x.pathname; } catch (e) { return u || ""; }
  }

  async function pollActivity() {
    if (!selected) return;
    try {
      const r = await fetch("/api/activity?session=" + encodeURIComponent(selected), { credentials: "same-origin", headers: { "X-Citadel-CSRF": csrf } });
      if (!r.ok) return;
      const recs = (await r.json()).records || [];
      const ol = $("activity");
      ol.replaceChildren();
      for (const rec of recs.slice().reverse()) {
        const li = document.createElement("li");
        const t = document.createElement("span");
        t.className = "t";
        t.textContent = new Date(rec.created_at).toLocaleTimeString();
        const who = document.createElement("span");
        who.className = "who";
        who.textContent = rec.actor === "user" ? "You" : (rec.agent || "Agent");
        const what = document.createElement("span");
        let detail = rec.url ? hostPath(rec.url) : (rec.selector || "");
        if (rec.text_len) detail = rec.text_len + " characters";
        what.textContent = rec.intended_action + (detail ? " " + detail : "");
        const out = document.createElement("span");
        out.textContent = rec.resulting_action + (rec.error ? ": " + rec.error : "");
        if (rec.resulting_action === "refused" || rec.resulting_action === "error") out.className = "refused";
        li.append(t, who, what, out);
        ol.appendChild(li);
      }
    } catch (e) { /* owner gone; poll shows it */ }
  }

  poll();
  setInterval(poll, 1000);
  pollActivity();
  setInterval(pollActivity, 2000);
})();
