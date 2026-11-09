// Citadel local viewer. Plain JavaScript, no build step. Every value from the
// server is rendered with textContent, never as HTML.
"use strict";

(function () {
  const csrf = document.querySelector('meta[name="csrf"]').content;
  const $ = (id) => document.getElementById(id);
  let state = { agent: "", sessions: [] };
  let selected = null;
  let ws = null;
  let wsFor = null;
  let frameURL = null;
  let human = false;

  async function post(id, action, body) {
    const r = await fetch("/api/sessions/" + encodeURIComponent(id) + "/" + action, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-Citadel-CSRF": csrf },
      body: JSON.stringify(body || {}),
    });
    if (!r.ok) {
      let msg = r.statusText;
      try { msg = (await r.json()).error || msg; } catch (e) { /* not JSON */ }
      $("hint").textContent = msg;
    }
    poll();
  }

  async function poll() {
    try {
      const r = await fetch("/api/state", { credentials: "same-origin" });
      if (!r.ok) { $("hint").textContent = "This viewer link has expired. Run citadel local viewer for a new one."; return; }
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
      b.textContent = (s.goal || s.session_id) + (s.driver === "human" ? " (you)" : "");
      if (s.session_id === selected) b.className = "sel";
      b.onclick = () => { selected = s.session_id; render(); };
      tabs.appendChild(b);
    }
    const waitingAny = sessions.some((s) => (s.driver === "human" && s.handoff_reason) || s.approval_asked);
    document.title = waitingAny ? "● Citadel: waiting for you" : "Citadel viewer";

    const s = sessions.find((x) => x.session_id === selected);
    $("empty").hidden = !!s;
    $("session").hidden = !s;
    if (!s) { closeStream(); return; }

    human = s.driver === "human";
    $("session-goal").textContent = s.goal || "Browser session";
    $("session-driver").textContent = human ? "You are driving" : "Agent driving";
    $("session-driver").className = human ? "pill human" : "pill";
    $("session-domain").textContent = domainOf(s.url);
    $("session-grant").textContent = s.approval === "approved"
      ? "AI actions allowed until " + new Date(s.grant_expires_at).toLocaleTimeString()
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

    const showCard = s.approval_asked && s.approval !== "approved";
    $("approval").hidden = !showCard;
    $("approval-domain").textContent = "Session: " + (s.goal || s.session_id) + " on " + domainOf(s.url);
    $("revoke").hidden = s.approval !== "approved";

    $("drive").textContent = human ? "Hand back to agent" : "Take control";
    const addr = $("address");
    addr.disabled = !human;
    if (document.activeElement !== addr) addr.value = s.url || "";
    $("screen-wrap").className = human ? "driving" : "";
    $("hint").textContent = human ? "Click the picture and type; your keystrokes go only to this browser." : "";
    openStream(s.session_id);
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
  $("approve").onclick = () => selected && post(selected, "approve", { hours: Number($("approve-hours").value) });
  $("deny").onclick = () => selected && post(selected, "deny");
  $("revoke").onclick = () => selected && post(selected, "revoke");
  $("stop").onclick = () => selected && confirm("End this browser session? Its cookies are deleted.") && post(selected, "stop");
  $("address").addEventListener("keydown", (ev) => {
    if (ev.key === "Enter" && selected && human) {
      let url = $("address").value.trim();
      if (url && !/^https?:\/\//i.test(url)) url = "https://" + url;
      post(selected, "navigate", { url });
    }
  });

  poll();
  setInterval(poll, 1000);
})();
