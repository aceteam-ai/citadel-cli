import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";
import { createAuthController } from "./auth-controller";
import "./style.css";

const root = document.querySelector<HTMLDivElement>("#app");
if (!root) throw new Error("Missing app root");

// All asynchronous account-bound work lives in the production-used controller.
// Rendering has no independent IPC continuation or account-state assignment.
const controller = createAuthController({
  invoke: <T>(command: string, args?: Record<string, unknown>) => invoke<T>(command, args),
  listen: <T>(name: string, receive: (payload: T) => void) => listen<T>(name, (event) => receive(event.payload)),
  delay: (milliseconds: number) => new Promise<void>((resolve) => setTimeout(resolve, milliseconds)),
  operationId: () => crypto.randomUUID(),
  render,
});
const state = controller.state;

function text(tag: string, value: string, className = ""): HTMLElement {
  const element = document.createElement(tag);
  element.textContent = value;
  if (className) element.className = className;
  return element;
}
function button(label: string, click: () => void | Promise<void>, className = "", cleanup = false, accountContext = true): HTMLButtonElement {
  const element = document.createElement("button");
  element.type = "button";
  element.textContent = label;
  element.className = className;
  element.disabled = cleanup ? false : state.busy || !state.ready || state.unavailable;
  const guarded = accountContext ? controller.bindAction(click) : click;
  element.addEventListener("click", () => { void guarded(); });
  return element;
}
function card(title: string, description: string): HTMLElement {
  const element = document.createElement("section");
  element.className = "card";
  element.append(text("h2", title), text("p", description, "muted"));
  return element;
}
function renderWelcome(main: HTMLElement): void {
  const welcome = document.createElement("div");
  welcome.className = "welcome card";
  welcome.append(
    text("div", "C", "mark"), text("p", "CITADEL", "eyebrow"),
    text("h1", "Your hardware. Your AI."),
    text("p", "Connect your computer or a Citadel box to AceTeam, then watch it work from one place.", "muted"),
    button("Sign in to AceTeam", () => controller.beginLogin(), "primary"),
  );
  if (state.canCancelLogin) welcome.append(button("Cancel sign in", () => controller.cancelLogin(), "quiet", true));
  if (!state.ready && !state.busy && !state.unavailable) welcome.append(button("Check sign-in status", () => controller.refresh(), "quiet", true));
  main.append(welcome);
}
function renderUnavailable(main: HTMLElement): void {
  const section = card("Sign-in unavailable", "Local sign-in is unavailable. Remote sign-in cleanup could not be confirmed. Review active sessions in AceTeam.");
  if (state.authCleanupNotice) section.append(text("p", state.authCleanupNotice, "notice"));
  if (state.authCleanupAvailable) {
    const cleanup = button(state.authCleanupPending ? "Cleanup request pending" : "Request sign-in cleanup", controller.bindAuthenticationCleanup(), "quiet", true, false);
    cleanup.disabled = state.authCleanupPending;
    section.append(cleanup);
  }
  main.append(section);
}
function renderMfa(main: HTMLElement): void {
  const section = card("Verify sign in", "Enter a code from the authenticator for this AceTeam account.");
  const factors = document.createElement("select");
  factors.setAttribute("aria-label", "Authenticator");
  for (const factor of state.auth.factors) {
    const option = document.createElement("option");
    option.value = factor.id;
    option.textContent = factor.friendly_name || "Authenticator";
    option.selected = factor.id === state.mfaFactor;
    factors.append(option);
  }
  const setFactor = controller.bindInput("mfaFactor");
  factors.addEventListener("change", () => setFactor(factors.value));
  const code = document.createElement("input");
  code.inputMode = "numeric";
  code.autocomplete = "one-time-code";
  code.maxLength = 8;
  code.placeholder = "Verification code";
  code.setAttribute("aria-label", "Verification code");
  code.value = state.mfaCode;
  const setCode = controller.bindInput("mfaCode");
  code.addEventListener("input", () => setCode(code.value));
  const controls = document.createElement("div");
  controls.className = "controls";
  controls.append(factors, code, button("Verify", () => controller.verifyMfa(), "primary"));
  if (state.canCancelLogin) controls.append(button("Cancel", () => controller.cancelLogin(), "quiet", true));
  section.append(controls);
  main.append(section);
}
function renderNodes(main: HTMLElement): void {
  const section = card("Your nodes", "Computers and boxes enrolled in your AceTeam organization.");
  section.append(button("Refresh", () => controller.refresh(), "quiet"));
  const list = document.createElement("div");
  list.className = "node-list";
  if (!state.nodes.length) list.append(text("p", "No nodes yet. Set up this computer or add a box below.", "empty"));
  else for (const node of state.nodes) {
    const row = document.createElement("div");
    row.className = "node-row";
    const identity = document.createElement("div");
    identity.append(text("strong", node.givenName || node.name || "Unnamed node"));
    if (node.user?.name) identity.append(text("small", node.user.name, "muted"));
    row.append(identity, text("span", node.online ? "Online" : "Offline", node.online ? "pill good" : "pill"));
    list.append(row);
  }
  section.append(list);
  main.append(section);
}
function renderLocal(main: HTMLElement): void {
  const section = card("This computer", "The bundled Citadel helper runs the node. Setup installs its background service.");
  if (state.local) {
    const grid = document.createElement("div");
    grid.className = "stats";
    const stats = [
      ["Node", state.local.nodeName || "This computer"],
      ["Fabric", state.local.connected ? "Connected" : "Not connected"],
      ["Backend", state.local.backendState || "Unknown"],
      ["CPU", `${Math.round(state.local.cpuPercent ?? 0)}%`],
      ["Memory", state.local.memoryTotal ? `${state.local.memoryUsed ?? "?"} / ${state.local.memoryTotal}` : "Unknown"],
      ["Jobs", `${state.local.inProgressJobs ?? 0} running, ${state.local.pendingJobs ?? 0} waiting`],
    ];
    for (const [label, value] of stats) {
      const stat = document.createElement("div");
      stat.append(text("small", label, "muted"), text("strong", value));
      grid.append(stat);
    }
    section.append(grid);
  }
  const controls = document.createElement("div");
  controls.className = "controls";
  const input = document.createElement("input");
  input.placeholder = "Name this computer";
  input.value = state.nodeName;
  input.maxLength = 64;
  input.setAttribute("aria-label", "Name this computer");
  const setName = controller.bindInput("nodeName");
  input.addEventListener("input", () => setName(input.value));
  const setup = button("Set up this computer", () => controller.startSetup(), "primary");
  setup.disabled ||= state.setupRunning;
  controls.append(input, setup);
  section.append(controls);
  if (state.setup) section.append(text("p", state.setup, "progress"));
  const service = document.createElement("details");
  service.append(text("summary", "Node controls and service details"));
  const actions = document.createElement("div");
  actions.className = "controls";
  actions.append(
    button("Start node", () => controller.serviceAction("start")),
    button("Stop node", () => controller.serviceAction("stop")),
    button("Show diagnostics", () => controller.diagnostics()),
  );
  service.append(actions, text("pre", state.service || "Service status is not available yet.", "service-status"));
  section.append(service);
  if (state.diagnostics) {
    const report = document.createElement("textarea");
    report.readOnly = true;
    report.value = state.diagnostics;
    report.setAttribute("aria-label", "Citadel diagnostics");
    section.append(report);
  }
  main.append(section);
}
function renderPair(main: HTMLElement): void {
  const section = card("Add a box", "Enter the code shown on your Citadel box. Once approved, it will appear in Your nodes.");
  const controls = document.createElement("div");
  controls.className = "controls";
  const input = document.createElement("input");
  input.placeholder = "ABCD-1234";
  input.maxLength = 9;
  input.value = state.pairCode;
  input.setAttribute("aria-label", "Box pairing code");
  const setCode = controller.bindInput("pairCode");
  input.addEventListener("input", () => setCode(input.value.toUpperCase()));
  controls.append(input, button("Pair box", () => controller.pairBox(), "primary"));
  section.append(controls);
  main.append(section);
}
function render(): void {
  const app = root!;
  app.replaceChildren();
  const header = document.createElement("header");
  header.append(text("span", "◆", "brand-icon"), text("strong", "CITADEL", "brand"));
  if (state.auth.signed_in) {
    header.append(text("span", state.auth.email || "Signed in", "account"));
    // Logout invalidates a long-running account action, rather than waiting on it.
    const logout = button("Sign out", () => controller.signOut(), "quiet", true);
    logout.disabled = !state.ready || state.closing || state.unavailable;
    header.append(logout);
  }
  const main = document.createElement("main");
  if (state.notice && !state.unavailable) main.append(text("p", state.notice, "notice"));
  if (state.unavailable) renderUnavailable(main);
  else if (state.auth.mfa_pending) renderMfa(main);
  else if (!state.auth.signed_in) renderWelcome(main);
  else {
    const intro = document.createElement("div");
    intro.className = "intro";
    intro.append(text("p", "YOUR NODE NETWORK", "eyebrow"), text("h1", "Good to see you."));
    main.append(intro);
    renderNodes(main);
    const columns = document.createElement("div");
    columns.className = "columns";
    renderLocal(columns);
    renderPair(columns);
    main.append(columns);
  }
  if (state.setupRunning) {
    // This machine-local ownership survives logout; no former-account text does.
    const setup = card("Local node setup", "A local node setup may still be running. Signing out does not stop it.");
    const cancel = button(state.setupCancellationRequested ? "Cancellation requested" : "Cancel local setup", controller.bindSetupCancellation(), "quiet", true, false);
    cancel.disabled = state.setupCancellationRequested;
    setup.append(cancel);
    main.append(setup);
  }
  app.append(header, main, text("footer", "Citadel runs on your hardware. Your node keeps running when you close this window.", "muted"));
}

render();
void controller.start();
setInterval(() => {
  if (!state.busy && !state.unavailable && (state.auth.signed_in || !state.ready)) void controller.refresh();
}, 30_000);
