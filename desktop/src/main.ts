import { invoke } from "@tauri-apps/api/core";
import { getCurrent, onOpenUrl } from "@tauri-apps/plugin-deep-link";
import "./style.css";

interface AuthView {
  signed_in: boolean;
  email: string | null;
}

interface FabricNode {
  id: number | string;
  name?: string;
  givenName?: string;
  online?: boolean;
  user?: { name?: string };
}

interface LocalStatus {
  nodeName?: string;
  connected?: boolean;
  version?: string;
  cpuPercent?: number;
  memoryPercent?: number;
  memoryUsed?: string;
  memoryTotal?: string;
  pendingJobs?: number;
  inProgressJobs?: number;
  failedJobs?: number;
  backendState?: string;
  lastUpdate?: string;
}

const root = document.querySelector<HTMLDivElement>("#app");
if (!root) throw new Error("Missing app root");

const state = {
  auth: { signed_in: false, email: null } as AuthView,
  nodes: [] as FabricNode[],
  local: null as LocalStatus | null,
  service: "",
  nodeName: "",
  pairCode: "",
  setup: "",
  notice: "",
  busy: false,
  diagnostics: "",
};
const handledCallbacks = new Set<string>();

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function text(tag: string, value: string, className = ""): HTMLElement {
  const element = document.createElement(tag);
  element.textContent = value;
  if (className) element.className = className;
  return element;
}

function button(label: string, click: () => void | Promise<void>, className = ""): HTMLButtonElement {
  const element = document.createElement("button");
  element.type = "button";
  element.textContent = label;
  element.className = className;
  element.disabled = state.busy;
  element.addEventListener("click", () => void click());
  return element;
}

function card(title: string, description: string): HTMLElement {
  const element = document.createElement("section");
  element.className = "card";
  element.append(text("h2", title), text("p", description, "muted"));
  return element;
}

function errorMessage(error: unknown): string {
  return typeof error === "string" ? error : "Something went wrong. Please try again.";
}

async function action(work: () => Promise<void>): Promise<void> {
  if (state.busy) return;
  state.busy = true;
  state.notice = "";
  render();
  try {
    await work();
  } catch (error) {
    state.notice = errorMessage(error);
  } finally {
    state.busy = false;
    render();
  }
}

async function refresh(): Promise<void> {
  state.auth = await invoke<AuthView>("session_status");
  if (state.auth.signed_in) {
    const results = await Promise.allSettled([
      invoke<FabricNode[]>("list_nodes"),
      invoke<LocalStatus>("local_status"),
      invoke<string>("service_status"),
    ]);
    if (results[0].status === "fulfilled") state.nodes = results[0].value;
    if (results[1].status === "fulfilled") state.local = results[1].value;
    if (results[2].status === "fulfilled") state.service = results[2].value;
  }
  render();
}

async function acceptCallback(url: string): Promise<void> {
  if (!url.startsWith("citadel://auth/callback?")) return;
  if (handledCallbacks.has(url)) return;
  handledCallbacks.add(url);
  await action(async () => {
    state.auth = await invoke<AuthView>("complete_login", { callbackUrl: url });
    state.notice = "Signed in. Your nodes are loading.";
    await refresh();
  });
}

function renderWelcome(main: HTMLElement): void {
  const welcome = document.createElement("div");
  welcome.className = "welcome card";
  welcome.append(
    text("div", "C", "mark"),
    text("p", "CITADEL", "eyebrow"),
    text("h1", "Your hardware. Your AI."),
    text("p", "Connect your computer or a Citadel box to AceTeam, then watch it work from one place.", "muted"),
    button("Sign in to AceTeam", () => action(async () => invoke("begin_login")), "primary"),
  );
  main.append(welcome);
}

function renderNodes(main: HTMLElement): void {
  const section = card("Your nodes", "Computers and boxes enrolled in your AceTeam organization.");
  section.append(button("Refresh", () => action(refresh), "quiet"));
  const list = document.createElement("div");
  list.className = "node-list";
  if (!state.nodes.length) {
    list.append(text("p", "No nodes yet. Set up this computer or add a box below.", "empty"));
  } else {
    for (const node of state.nodes) {
      const row = document.createElement("div");
      row.className = "node-row";
      const identity = document.createElement("div");
      identity.append(text("strong", node.givenName || node.name || "Unnamed node"));
      if (node.user?.name) identity.append(text("small", node.user.name, "muted"));
      row.append(identity, text("span", node.online ? "Online" : "Offline", node.online ? "pill good" : "pill"));
      list.append(row);
    }
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
  input.addEventListener("input", () => { state.nodeName = input.value; });
  controls.append(input, button("Set up this computer", () => action(async () => {
    await invoke("init_this_computer", { nodeName: state.nodeName || "Citadel Mac" });
    state.setup = "Registering this computer";
  }), "primary"));
  section.append(controls);
  if (state.setup) section.append(text("p", state.setup, "progress"));
  const service = document.createElement("details");
  service.append(text("summary", "Node controls and service details"));
  const actions = document.createElement("div");
  actions.className = "controls";
  actions.append(
    button("Start node", () => action(async () => { await invoke("service_action", { action: "start" }); await refresh(); })),
    button("Stop node", () => action(async () => { await invoke("service_action", { action: "stop" }); await refresh(); })),
    button("Show diagnostics", () => action(async () => { state.diagnostics = await invoke<string>("diagnostics"); })),
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
  input.addEventListener("input", () => { state.pairCode = input.value.toUpperCase(); });
  controls.append(input, button("Pair box", () => action(async () => {
    const previous = new Map(state.nodes.map((node) => [String(node.id), node.online]));
    await invoke("approve_device", { code: state.pairCode });
    state.notice = "Box approved. Waiting for it to come online.";
    state.pairCode = "";
    for (let attempt = 0; attempt < 12; attempt += 1) {
      await delay(5_000);
      await refresh();
      if (state.nodes.some((node) => node.online && !previous.get(String(node.id)))) {
        state.notice = "Box ready. It is online in your node list.";
        return;
      }
    }
    state.notice = "Box approved but not online yet. Refresh the node list in a moment.";
  }), "primary"));
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
    header.append(button("Sign out", () => action(async () => {
      await invoke("sign_out");
      state.auth = { signed_in: false, email: null };
      state.nodes = [];
      state.local = null;
    }), "quiet"));
  }
  const main = document.createElement("main");
  if (state.notice) main.append(text("p", state.notice, "notice"));
  if (!state.auth.signed_in) {
    renderWelcome(main);
  } else {
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
  app.append(header, main, text("footer", "Citadel runs on your hardware. Your node keeps running when you close this window.", "muted"));
}

void onOpenUrl((urls) => { for (const url of urls) void acceptCallback(url); });
void getCurrent().then((urls) => { for (const url of urls ?? []) void acceptCallback(url); });
void refresh().catch((error) => { state.notice = errorMessage(error); render(); });
setInterval(() => { if (state.auth.signed_in && !state.busy) void refresh().catch(() => {}); }, 30_000);

import { listen } from "@tauri-apps/api/event";
void listen<{ stage: string; message: string }>("citadel:init", (event) => {
  state.setup = event.payload.message;
  if (event.payload.stage === "error") state.notice = event.payload.message;
  if (event.payload.stage === "ready") {
    void (async () => {
      for (let attempt = 0; attempt < 10; attempt += 1) {
        await delay(2_000);
        await refresh();
        if (state.nodes.some((node) => node.online && (node.givenName || node.name) === (state.nodeName || "Citadel Mac"))) {
          state.setup = "This computer is online in your node list.";
          render();
          return;
        }
      }
      state.setup = "Citadel started. It is still connecting to the Fabric.";
      render();
    })();
  }
  render();
});
