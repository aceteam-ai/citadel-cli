// AUTH1 source candidate. No imports, tests or native actions have been run.
const MAX = 18446744073709551615n;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const UNAVAILABLE = "Local sign-in is unavailable. Remote sign-in cleanup could not be confirmed. Review active sessions in AceTeam.";
const CLEANUP_ACKNOWLEDGED = "Cleanup request acknowledged. This does not confirm that all remote sessions were revoked.";

export interface CancelTarget { expected_auth_epoch: string; operation_id: string }
export interface AuthView {
  auth_epoch: string;
  view_revision: string;
  signed_in: boolean;
  email: string | null;
  mfa_pending: boolean;
  factors: { id: string; friendly_name: string | null }[];
  cancel_target: CancelTarget | null;
}
export interface AuthOutcome {
  operation_id: string;
  originating_auth_epoch: string;
  reserved_auth_epoch: string | null;
  view: AuthView;
  error: string | null;
  superseded: boolean;
}
export interface InitEvent { auth_epoch: string; setup_operation_id: string; stage: string; message: string }
export interface FabricNode {
  id: number | string;
  name?: string;
  givenName?: string;
  online?: boolean;
  user?: { name?: string };
}
export interface LocalStatus {
  nodeName?: string; connected?: boolean; version?: string; cpuPercent?: number;
  memoryPercent?: number; memoryUsed?: string; memoryTotal?: string;
  pendingJobs?: number; inProgressJobs?: number; failedJobs?: number;
  backendState?: string; lastUpdate?: string;
}
interface Presentation {
  signed_in: boolean; email: string | null; mfa_pending: boolean;
  factors: { id: string; friendly_name: string | null }[];
}
export interface DesktopState {
  auth: Presentation;
  ready: boolean;
  unavailable: boolean;
  nodes: FabricNode[];
  local: LocalStatus | null;
  service: string;
  nodeName: string;
  pairCode: string;
  setup: string;
  setupRunning: boolean;
  setupCancellationRequested: boolean;
  notice: string;
  busy: boolean;
  closing: boolean;
  diagnostics: string;
  loginStarted: boolean;
  canCancelLogin: boolean;
  mfaFactor: string;
  mfaCode: string;
  authCleanupAvailable: boolean;
  authCleanupPending: boolean;
  authCleanupNotice: string;
}
export interface ControllerDependencies {
  invoke<T>(command: string, args?: Record<string, unknown>): Promise<T>;
  listen<T>(event: string, receive: (payload: T) => void): Promise<() => void>;
  delay(milliseconds: number): Promise<void>;
  operationId(): string;
  render(): void;
}
interface ActionOwner { revision: bigint }
interface Context { intent: bigint; epoch: string | null; owner: ActionOwner | null; refresh?: bigint }
interface Transition { owner: ActionOwner; operation: string; expected: string; kind: string }
interface CleanupBinding { epoch: string; viewRevision: string }
interface CleanupOwner { binding: CleanupBinding; operation: string }
interface SetupOwner {
  operation: string; epoch: string; intent: bigint; nodeName: string;
  terminal: boolean; polling: boolean; cancelling: boolean;
}
const signedOut = (): Presentation => ({ signed_in: false, email: null, mfa_pending: false, factors: [] });
const object = (value: unknown): value is Record<string, unknown> => !!value && typeof value === "object" && !Array.isArray(value);
const nullableString = (value: unknown): value is string | null => value === null || typeof value === "string";
const errorMessage = (error: unknown): string => typeof error === "string" ? error : "Something went wrong. Please try again.";

function counter(value: unknown): bigint | null {
  if (typeof value !== "string" || !/^[1-9][0-9]{0,19}$/.test(value)) return null;
  const parsed = BigInt(value);
  return parsed <= MAX ? parsed : null;
}
function viewFrom(value: unknown): AuthView | null {
  if (!object(value)) return null;
  const epoch = counter(value.auth_epoch), revision = counter(value.view_revision);
  if (epoch === null || revision === null || typeof value.signed_in !== "boolean" ||
      typeof value.mfa_pending !== "boolean" || !nullableString(value.email) || !Array.isArray(value.factors)) return null;
  const factors: AuthView["factors"] = [];
  for (const factor of value.factors) {
    if (!object(factor) || typeof factor.id !== "string" || !factor.id ||
        !nullableString(factor.friendly_name) || factors.some((prior) => prior.id === factor.id)) return null;
    factors.push({ id: factor.id, friendly_name: factor.friendly_name });
  }
  if (value.signed_in && value.mfa_pending || !value.signed_in && value.email !== null ||
      !value.mfa_pending && factors.length !== 0 || value.mfa_pending && factors.length === 0) return null;
  let target: CancelTarget | null = null;
  if (value.cancel_target !== null) {
    if (!object(value.cancel_target) || counter(value.cancel_target.expected_auth_epoch) === null ||
        typeof value.cancel_target.operation_id !== "string" || !UUID.test(value.cancel_target.operation_id)) return null;
    const original = counter(value.cancel_target.expected_auth_epoch)!;
    if (original === MAX || original + (value.signed_in ? 2n : 1n) !== epoch) return null;
    target = { expected_auth_epoch: value.cancel_target.expected_auth_epoch as string, operation_id: value.cancel_target.operation_id };
  }
  if (epoch === MAX || revision === MAX) {
    if (epoch !== MAX || revision !== MAX || value.signed_in || value.mfa_pending || value.email !== null || factors.length || target) return null;
  } else if (revision < epoch) return null;
  return {
    auth_epoch: value.auth_epoch as string, view_revision: value.view_revision as string,
    signed_in: value.signed_in, email: value.email, mfa_pending: value.mfa_pending,
    factors, cancel_target: target,
  };
}
function fingerprint(view: AuthView): string {
  // Only the validated canonical field projection participates in equality.
  return JSON.stringify(view);
}
function outcomeFrom(value: unknown): AuthOutcome | null {
  if (!object(value) || typeof value.operation_id !== "string" || !UUID.test(value.operation_id) ||
      counter(value.originating_auth_epoch) === null || value.originating_auth_epoch === String(MAX) ||
      !(value.reserved_auth_epoch === null || counter(value.reserved_auth_epoch) !== null && value.reserved_auth_epoch !== String(MAX)) ||
      !nullableString(value.error) || typeof value.superseded !== "boolean") return null;
  const view = viewFrom(value.view);
  return view ? {
    operation_id: value.operation_id, originating_auth_epoch: value.originating_auth_epoch as string,
    reserved_auth_epoch: value.reserved_auth_epoch as string | null, view,
    error: value.error, superseded: value.superseded,
  } : null;
}

export class AuthController {
  readonly state: DesktopState = {
    auth: signedOut(), ready: false, unavailable: false, nodes: [], local: null,
    service: "", nodeName: "", pairCode: "", setup: "", setupRunning: false,
    setupCancellationRequested: false, notice: "", busy: false, closing: false,
    diagnostics: "", loginStarted: false, canCancelLogin: false, mfaFactor: "", mfaCode: "",
    authCleanupAvailable: false, authCleanupPending: false, authCleanupNotice: "",
  };
  private dependencies: ControllerDependencies;
  private accepted: AuthView | null = null;
  private intentRevision = 0n;
  private refreshRevision = 0n;
  private actionRevision = 0n;
  private actionOwner: ActionOwner | null = null;
  private transition: Transition | null = null;
  private pendingBegin: CancelTarget | null = null;
  private authOperation: string | null = null;
  private setupOwner: SetupOwner | null = null;
  private eventDirty = false;
  private started = false;
  private subscribed = false;
  // Process-local cleanup authority; restarting is not proof of remote cleanup.
  private cleanupLatched = false;
  private cleanupBinding: CleanupBinding | null = null;
  private cleanupOwner: CleanupOwner | null = null;

  constructor(dependencies: ControllerDependencies) { this.dependencies = dependencies; }
  private paint(): void { this.dependencies.render(); }
  private context(owner: ActionOwner | null = null): Context {
    return { intent: this.intentRevision, epoch: this.accepted?.auth_epoch ?? null, owner };
  }
  private current(context: Context): boolean {
    return !this.state.unavailable && context.intent === this.intentRevision &&
      context.epoch === (this.accepted?.auth_epoch ?? null) &&
      (context.owner === null || context.owner === this.actionOwner) &&
      (context.refresh === undefined || context.refresh === this.refreshRevision);
  }
  private clearPresentation(): void {
    Object.assign(this.state, {
      auth: signedOut(), nodes: [], local: null, service: "", diagnostics: "",
      nodeName: "", pairCode: "", mfaFactor: "", mfaCode: "", setup: "",
      notice: "", loginStarted: false, canCancelLogin: false,
    });
    // Machine-local setup ownership is deliberately NOT cleared here.
  }
  private disable(): void {
    if (!this.cleanupLatched) {
      this.cleanupLatched = true;
      this.cleanupBinding = this.accepted ? {
        epoch: this.accepted.auth_epoch, viewRevision: this.accepted.view_revision,
      } : null;
    }
    this.clearPresentation();
    this.state.unavailable = true;
    this.state.ready = false;
    this.state.notice = UNAVAILABLE;
    this.state.busy = false;
    this.state.closing = false;
    this.actionOwner = null;
    this.transition = null;
    this.pendingBegin = null;
    this.authOperation = null;
    this.state.authCleanupAvailable = this.subscribed && this.cleanupBinding !== null;
    this.state.authCleanupPending = this.cleanupOwner !== null;
    this.paint();
  }
  private increment(kind: "intentRevision" | "refreshRevision" | "actionRevision"): bigint | null {
    const next = this[kind] + 1n;
    if (next >= MAX) { this.disable(); return null; }
    this[kind] = next;
    return next;
  }
  private id(): string {
    const id = this.dependencies.operationId();
    if (!UUID.test(id)) throw "Could not start a local operation.";
    return id;
  }
  private acceptable(view: AuthView): boolean {
    if (this.state.unavailable) return false;
    if (!this.accepted) return true;
    const epoch = BigInt(view.auth_epoch), oldEpoch = BigInt(this.accepted.auth_epoch);
    const revision = BigInt(view.view_revision), oldRevision = BigInt(this.accepted.view_revision);
    if (epoch < oldEpoch || revision < oldRevision || epoch > oldEpoch && revision <= oldRevision) return false;
    if (epoch === oldEpoch && view.signed_in !== this.accepted.signed_in) return false;
    return epoch !== oldEpoch || revision !== oldRevision || fingerprint(view) === fingerprint(this.accepted);
  }
  private adopt(view: AuthView, keepOwner: ActionOwner | null): boolean {
    if (!this.acceptable(view)) return false;
    if (view.auth_epoch === String(MAX)) { this.accepted = view; this.disable(); return true; }
    if (this.accepted?.auth_epoch !== view.auth_epoch) {
      this.clearPresentation();
      if (this.actionOwner !== keepOwner) {
        this.actionOwner = null;
        this.state.busy = false;
      }
    }
    this.accepted = view;
    this.state.auth = {
      signed_in: view.signed_in, email: view.email, mfa_pending: view.mfa_pending,
      factors: view.factors.map((factor) => ({ ...factor })),
    };
    this.pendingBegin = view.cancel_target ? { ...view.cancel_target } : null;
    this.state.loginStarted = !!this.pendingBegin && !view.signed_in && !view.mfa_pending;
    this.state.canCancelLogin = !!this.pendingBegin && !this.state.closing;
    this.state.mfaFactor = view.factors.some((factor) => factor.id === this.state.mfaFactor)
      ? this.state.mfaFactor : view.factors[0]?.id ?? "";
    this.state.ready = true;
    return true;
  }
  private async bound<T>(context: Context, command: string, args: Record<string, unknown>, valid: (value: unknown) => value is T): Promise<{ value: T } | null> {
    if (!this.current(context) || !context.epoch || !this.state.auth.signed_in) return null;
    const raw = await this.dependencies.invoke<unknown>(command, { ...args, expectedAuthEpoch: context.epoch });
    if (!this.current(context)) return null;
    if (!object(raw) || raw.auth_epoch !== context.epoch || counter(raw.view_revision) === null ||
        raw.view_revision === String(MAX) || BigInt(raw.view_revision as string) < BigInt(this.accepted!.view_revision) || !valid(raw.value)) {
      throw "Citadel returned an invalid or stale node result. Refresh to try again.";
    }
    return { value: raw.value };
  }
  private presentNotification(context: Context, notification: AuthOutcome | null): void {
    if (!notification || !this.current(context) || !this.accepted) return;
    const matchesOperation = notification.operation_id === this.authOperation || notification.operation_id === this.accepted.cancel_target?.operation_id;
    if (matchesOperation && fingerprint(notification.view) === fingerprint(this.accepted) && notification.error) {
      this.state.notice = notification.error;
      this.paint();
    }
  }
  private async refreshFrom(parent: Context, control = false, notification: AuthOutcome | null = null): Promise<boolean> {
    if (!this.subscribed || !this.current(parent) || this.transition && !control) return false;
    const revision = this.increment("refreshRevision");
    if (revision === null) return false;
    const context: Context = { ...parent, refresh: revision };
    try {
      const raw = await this.dependencies.invoke<unknown>("session_status");
      if (!this.current(context)) return false;
      const view = viewFrom(raw);
      if (!view || !this.adopt(view, control ? parent.owner : null)) throw "Citadel returned invalid or out-of-order sign-in state.";
      if (this.state.unavailable) return false;
      context.epoch = view.auth_epoch;
      if (control) parent.epoch = view.auth_epoch;
      if (!this.current(context)) { this.paint(); return false; }
      this.paint();
      if (!view.signed_in) { this.presentNotification(context, notification); return true; }
      const results = await Promise.allSettled([
        this.bound(context, "list_nodes", {}, (value): value is FabricNode[] => Array.isArray(value)),
        this.bound(context, "local_status", {}, (value): value is LocalStatus => object(value)),
        this.bound(context, "service_status", {}, (value): value is string => typeof value === "string"),
      ]);
      if (!this.current(context)) return false;
      if (results[0].status === "fulfilled" && results[0].value) this.state.nodes = results[0].value.value;
      if (results[1].status === "fulfilled" && results[1].value) this.state.local = results[1].value.value;
      if (results[2].status === "fulfilled" && results[2].value) this.state.service = results[2].value.value;
      this.presentNotification(context, notification);
      this.paint();
      return true;
    } catch (error) {
      // Publish before leaving this exact child scope. A weaker caller context
      // cannot safely republish after its await introduces another microtask.
      if (this.current(context)) {
        if (control) this.state.ready = false;
        this.state.notice = errorMessage(error);
        this.paint();
      }
      return false;
    }
  }
  async start(): Promise<void> {
    if (this.started) return;
    this.started = true;
    // Preserve the existing automatic machine-local command independently of
    // authentication/subscription success. Its notice never borrows later context.
    void this.reconcileHelper();
    try {
      // No status bootstrap or enabled action precedes BOTH subscriptions.
      await this.dependencies.listen<unknown>("citadel:auth", (event) => { void this.authEvent(event); });
      await this.dependencies.listen<unknown>("citadel:init", (event) => { void this.initEvent(event); });
      this.subscribed = true;
      await this.refresh();
    } catch { this.disable(); }
  }
  async refresh(): Promise<void> {
    await this.refreshFrom(this.context());
  }
  private async reconcileHelper(): Promise<void> {
    const context = this.context();
    try {
      const result = await this.dependencies.invoke<{ status: string }>("reconcile_desktop_helper");
      if (!this.current(context)) return;
      if (result.status === "restart_failed") this.state.notice = "Citadel updated its node helper, but the service could not restart. Try Service > Start or open Diagnostics.";
      else if (["stage_failed", "inspect_failed"].includes(result.status)) this.state.notice = "Citadel could not prepare the node service. Open Diagnostics for help.";
      this.paint();
    } catch {
      if (this.current(context)) { this.state.notice = "Citadel could not check the node service. Open Diagnostics for help."; this.paint(); }
    }
  }
  private async control(kind: string, expected: string, args: Record<string, unknown>, pending: CancelTarget | null = null): Promise<void> {
    if (!this.subscribed || this.state.unavailable || this.state.closing || this.transition && kind !== "cancel_login") return;
    let operation: string;
    try { operation = this.id(); } catch (error) { this.state.notice = errorMessage(error); this.paint(); return; }
    if (this.increment("intentRevision") === null) return;
    const revision = this.increment("actionRevision");
    if (revision === null) return;
    const owner = { revision };
    const context = this.context(owner);
    this.clearPresentation();
    this.state.ready = false;
    this.state.busy = true;
    this.state.closing = kind === "cancel_login" || kind === "sign_out";
    this.actionOwner = owner;
    this.authOperation = operation;
    this.transition = { owner, operation, expected, kind };
    this.pendingBegin = kind === "begin_login" ? { expected_auth_epoch: expected, operation_id: operation } : pending;
    this.state.loginStarted = kind === "begin_login";
    this.state.canCancelLogin = !!this.pendingBegin && !this.state.closing;
    this.paint();
    let outcome: AuthOutcome | null = null;
    let rejected = false;
    try {
      const raw = await this.dependencies.invoke<unknown>(kind, { ...args, expectedAuthEpoch: expected, operationId: operation });
      if (!this.current(context)) return;
      outcome = outcomeFrom(raw);
      if (!outcome || outcome.operation_id !== operation || outcome.originating_auth_epoch !== expected) {
        rejected = true;
        outcome = null;
      }
    } catch { rejected = true; }
    try {
      if (!this.current(context)) return;
      // Acknowledgements are not assignments: even an ambiguous rejection needs status.
      this.eventDirty = false;
      const reconciled = await this.refreshFrom(context, true);
      if (!this.current(context) || !reconciled) return;
      if (outcome && !outcome.superseded && this.accepted && fingerprint(outcome.view) === fingerprint(this.accepted)) {
        if (outcome.error) this.state.notice = outcome.error;
        else if (kind === "begin_login" && !this.state.auth.signed_in && !this.state.auth.mfa_pending) this.state.notice = "Finish sign in in your browser.";
      } else if (rejected && this.accepted?.auth_epoch === expected) {
        this.state.notice = "Could not complete the sign-in change. Check sign-in status before trying again.";
      }
    } catch (error) {
      if (this.current(context)) { this.state.ready = false; this.state.notice = errorMessage(error); }
    } finally {
      if (this.current(context) && this.actionOwner === owner) {
        this.actionOwner = null;
        this.transition = null;
        this.state.busy = false;
        this.state.closing = false;
        this.state.canCancelLogin = !!this.pendingBegin;
        this.paint();
        if (this.eventDirty) { this.eventDirty = false; void this.refresh(); }
      }
    }
  }
  async beginLogin(): Promise<void> {
    if (!this.state.ready || this.state.auth.signed_in || !this.accepted) return;
    await this.control("begin_login", this.accepted.auth_epoch, {});
  }
  async cancelLogin(): Promise<void> {
    const target = this.pendingBegin;
    if (!target || this.state.closing) return;
    await this.control("cancel_login", target.expected_auth_epoch, { targetOperationId: target.operation_id }, { ...target });
  }
  async verifyMfa(): Promise<void> {
    if (!this.state.ready || !this.state.auth.mfa_pending || !this.accepted) return;
    const factorId = this.state.mfaFactor, code = this.state.mfaCode;
    await this.control("verify_mfa", this.accepted.auth_epoch, { factorId, code }, this.pendingBegin ? { ...this.pendingBegin } : null);
  }
  async signOut(): Promise<void> {
    if (!this.state.ready || !this.state.auth.signed_in || !this.accepted) return;
    await this.control("sign_out", this.accepted.auth_epoch, {});
  }
  private cleanupCurrent(owner: CleanupOwner): boolean {
    return this.subscribed && this.state.unavailable && this.cleanupOwner === owner &&
      this.cleanupBinding === owner.binding;
  }
  private cleanupAcknowledged(raw: unknown, owner: CleanupOwner): boolean {
    if (!object(raw) || raw.operation_id !== owner.operation ||
        raw.originating_auth_epoch !== owner.binding.epoch || raw.superseded !== false ||
        !nullableString(raw.error)) return false;
    const view = viewFrom(raw.view);
    if (!view || view.signed_in || view.email !== null || view.mfa_pending ||
        view.factors.length !== 0 || view.cancel_target !== null) return false;
    const terminal = view.auth_epoch === String(MAX) && view.view_revision === String(MAX);
    const expected = BigInt(owner.binding.epoch);
    let shape = false;
    if (expected === MAX) shape = raw.reserved_auth_epoch === null && terminal;
    else if (raw.reserved_auth_epoch === null) shape = terminal;
    else {
      const reserved = counter(raw.reserved_auth_epoch);
      shape = reserved !== null && reserved < MAX && reserved === expected + 1n &&
        (terminal || view.auth_epoch === String(reserved) &&
          BigInt(view.view_revision) >= reserved &&
          BigInt(view.view_revision) > BigInt(owner.binding.viewRevision));
    }
    // This command-specific result never becomes an AuthView/event assignment.
    return shape && raw.error === null;
  }
  private async cleanupAuthenticationFor(binding: CleanupBinding): Promise<void> {
    if (!this.subscribed || !this.state.unavailable || this.cleanupBinding !== binding || this.cleanupOwner) return;
    let operation: string;
    try { operation = this.id(); }
    catch {
      this.state.authCleanupNotice = UNAVAILABLE;
      this.paint();
      return;
    }
    const owner: CleanupOwner = { binding, operation };
    this.cleanupOwner = owner;
    this.state.authCleanupPending = true;
    this.state.authCleanupNotice = "Cleanup request pending. Remote sign-in cleanup is not confirmed.";
    this.paint();
    let acknowledged = false;
    try {
      const raw = await this.dependencies.invoke<unknown>("sign_out", {
        expectedAuthEpoch: binding.epoch, operationId: operation,
      });
      if (!this.cleanupCurrent(owner)) return;
      acknowledged = this.cleanupAcknowledged(raw, owner);
    } catch { /* An ambiguous failure cannot confer replacement authority. */ }
    if (!this.cleanupCurrent(owner)) return;
    this.state.authCleanupNotice = acknowledged ? CLEANUP_ACKNOWLEDGED : UNAVAILABLE;
    this.state.authCleanupPending = false;
    this.cleanupOwner = null;
    // Finish all exact-owner writes before rendering can admit an explicit retry.
    this.paint();
  }
  async cleanupAuthentication(): Promise<void> {
    const binding = this.cleanupBinding;
    if (binding) await this.cleanupAuthenticationFor(binding);
  }
  bindAuthenticationCleanup(): () => Promise<void> {
    const binding = this.cleanupBinding;
    return async () => { if (binding) await this.cleanupAuthenticationFor(binding); };
  }
  private async authEvent(raw: unknown): Promise<void> {
    const event = outcomeFrom(raw);
    if (!event || event.superseded || !this.acceptable(event.view)) return;
    if (this.transition) { this.eventDirty = true; return; }
    await this.refreshFrom(this.context(), false, event);
  }
  bindInput(field: "nodeName" | "pairCode" | "mfaFactor" | "mfaCode"): (value: string) => void {
    const context = this.context();
    return (value) => { if (this.current(context) && this.state.ready) this.state[field] = value; };
  }
  bindAction(work: () => void | Promise<void>): () => void | Promise<void> {
    const context = this.context();
    return () => { if (this.current(context)) return work(); };
  }
  bindSetupCancellation(): () => Promise<void> {
    const setup = this.setupOwner;
    // Cleanup may outlive account presentation, but not this exact local owner.
    return async () => { if (setup && this.setupOwner === setup && !setup.terminal) await this.cancelSetup(); };
  }
  private async action(work: (context: Context) => Promise<void>): Promise<void> {
    if (!this.subscribed || !this.state.ready || !this.state.auth.signed_in || this.state.busy || this.state.unavailable) return;
    const revision = this.increment("actionRevision");
    if (revision === null) return;
    const owner = { revision }, context = this.context(owner);
    this.actionOwner = owner;
    this.state.busy = true;
    this.state.notice = "";
    this.paint();
    try { await work(context); }
    catch (error) { if (this.current(context)) this.state.notice = errorMessage(error); }
    finally {
      if (this.current(context) && this.actionOwner === owner) {
        this.actionOwner = null;
        this.state.busy = false;
        this.paint();
      }
    }
  }
  async serviceAction(action: "start" | "stop"): Promise<void> {
    await this.action(async (context) => {
      const result = await this.bound(context, "service_action", { action }, (value): value is null => value === null);
      if (!result || !this.current(context)) return;
      await this.refreshFrom(context);
    });
  }
  async diagnostics(): Promise<void> {
    await this.action(async (context) => {
      const result = await this.bound(context, "diagnostics", {}, (value): value is string => typeof value === "string");
      if (result && this.current(context)) this.state.diagnostics = result.value;
    });
  }
  async pairBox(): Promise<void> {
    const code = this.state.pairCode;
    await this.action(async (context) => {
      const previous = new Map(this.state.nodes.map((node) => [String(node.id), node.online]));
      const result = await this.bound(context, "approve_device", { code }, (value): value is null => value === null);
      if (!result || !this.current(context)) return;
      this.state.notice = "Box approved. Waiting for it to come online.";
      this.state.pairCode = "";
      this.paint();
      for (let attempt = 0; attempt < 12; attempt += 1) {
        await this.dependencies.delay(5_000);
        if (!this.current(context)) return;
        if (!await this.refreshFrom(context) || !this.current(context)) return;
        if (this.state.nodes.some((node) => node.online && !previous.get(String(node.id)))) {
          this.state.notice = "Box ready. It is online in your node list.";
          return;
        }
      }
      if (this.current(context)) this.state.notice = "Box approved but not online yet. Refresh the node list in a moment.";
    });
  }
  async startSetup(): Promise<void> {
    if (this.setupOwner && !this.setupOwner.terminal) return;
    const nodeName = this.state.nodeName.trim() || "Citadel Mac";
    if (nodeName.length > 64 || /[\u0000-\u001f\u007f]/.test(nodeName)) return;
    await this.action(async (context) => {
      const setup: SetupOwner = { operation: this.id(), epoch: context.epoch!, intent: context.intent, nodeName, terminal: false, polling: false, cancelling: false };
      // Publish ownership BEFORE invoke: even a terminal event can arrive first.
      this.setupOwner = setup;
      this.state.setupRunning = true;
      this.state.setupCancellationRequested = false;
      this.state.setup = "Registering this computer";
      this.paint();
      try {
        const result = await this.bound(context, "init_this_computer", { setupOperationId: setup.operation, nodeName }, (value): value is null => value === null);
        if (!result || !this.current(context) || this.setupOwner !== setup || setup.terminal) return;
        this.state.setup = "Registering this computer";
      } catch (error) {
        // An IPC rejection may follow task admission. It is NOT stop proof.
        if (this.current(context) && this.setupOwner === setup && !setup.terminal) this.state.setup = "Local setup may still be running. Cancel this setup before starting another.";
        throw error;
      }
    });
  }
  async cancelSetup(): Promise<void> {
    const setup = this.setupOwner;
    if (!setup || setup.terminal || setup.cancelling) return;
    setup.cancelling = true;
    this.state.setupCancellationRequested = true;
    this.paint();
    try {
      await this.dependencies.invoke<void>("cancel_init", { originatingAuthEpoch: setup.epoch, setupOperationId: setup.operation });
      if (this.setupOwner !== setup || setup.terminal) return;
      // Acknowledgement cannot establish process quiescence or clear ownership.
      if (setup.intent === this.intentRevision && setup.epoch === this.accepted?.auth_epoch) this.state.setup = "Node setup cancellation requested.";
    } catch {
      if (this.setupOwner === setup && !setup.terminal) {
        setup.cancelling = false;
        this.state.setupCancellationRequested = false;
      }
    }
    if (this.setupOwner === setup) this.paint();
  }
  private async initEvent(raw: unknown): Promise<void> {
    if (!object(raw) || typeof raw.auth_epoch !== "string" || typeof raw.setup_operation_id !== "string" ||
        typeof raw.stage !== "string" || typeof raw.message !== "string") return;
    const setup = this.setupOwner;
    if (!setup || setup.terminal || raw.auth_epoch !== setup.epoch || raw.setup_operation_id !== setup.operation) return;
    const terminal = ["error", "ready", "cancelled"].includes(raw.stage);
    if (terminal) {
      setup.terminal = true;
      this.state.setupRunning = false;
      this.state.setupCancellationRequested = false;
    }
    // Retiring this EXACT local owner is allowed after logout. Its message is not.
    const context = this.context();
    if (setup.intent !== context.intent || setup.epoch !== context.epoch || !this.current(context) || !this.state.auth.signed_in) {
      if (terminal) this.paint();
      return;
    }
    this.state.setup = raw.message;
    if (raw.stage === "error") this.state.notice = raw.message;
    this.paint();
    if (raw.stage !== "ready" || setup.polling) return;
    setup.polling = true;
    try {
      for (let attempt = 0; attempt < 10; attempt += 1) {
        await this.dependencies.delay(2_000);
        if (!this.current(context) || this.setupOwner !== setup) return;
        if (!await this.refreshFrom(context) || !this.current(context) || this.setupOwner !== setup) return;
        if (this.state.nodes.some((node) => node.online && (node.givenName || node.name) === setup.nodeName)) {
          this.state.setup = "This computer is online in your node list.";
          this.paint();
          return;
        }
      }
      if (this.current(context) && this.setupOwner === setup) { this.state.setup = "Citadel started. It is still connecting to the Fabric."; this.paint(); }
    } catch (error) { if (this.current(context) && this.setupOwner === setup) { this.state.notice = errorMessage(error); this.paint(); } }
  }
}

export function createAuthController(dependencies: ControllerDependencies): AuthController {
  return new AuthController(dependencies);
}
