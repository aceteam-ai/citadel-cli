// AUTH1 source-only regression candidate. UNRUN. No native or backend access.
import test from "node:test";
import assert from "node:assert/strict";
import { createAuthController } from "../src/auth-controller.ts";

const MAX = 18446744073709551615n;
const id = (n) => `00000000-0000-4000-8000-${String(n).padStart(12, "0")}`;
const view = (epoch = "1", revision = epoch, email = null, extra = {}) => ({
  auth_epoch: epoch, view_revision: revision, signed_in: email !== null,
  email, mfa_pending: false, factors: [], cancel_target: null, ...extra,
});
const terminal = () => view(String(MAX), String(MAX));
const pending = (epoch, revision, original, operation) => view(epoch, revision, null, {
  cancel_target: { expected_auth_epoch: original, operation_id: operation },
});
const ack = (args, snapshot, extra = {}) => ({
  operation_id: args.operationId, originating_auth_epoch: args.expectedAuthEpoch,
  reserved_auth_epoch: snapshot.auth_epoch, view: snapshot, error: null,
  superseded: false, ...extra,
});
function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
async function flush() { for (let n = 0; n < 24; n += 1) await Promise.resolve(); }

function fixture(initial = view()) {
  const h = {
    snapshot: initial, nodes: [], local: { nodeName: "local" }, service: "service",
    calls: [], renders: [], events: new Map(), queues: new Map(), delays: [],
    subscriptions: [], subscriptionGates: new Map(), serial: 0,
  };
  h.enqueue = (command, reply) => {
    const queue = h.queues.get(command) ?? [];
    queue.push(reply);
    h.queues.set(command, queue);
  };
  h.bound = (value, snapshot = h.snapshot) => ({ auth_epoch: snapshot.auth_epoch, view_revision: snapshot.view_revision, value });
  h.count = (command) => h.calls.filter((call) => call.command === command).length;
  h.last = (command) => h.calls.filter((call) => call.command === command).at(-1);
  h.emit = (event, payload) => h.events.get(event)?.(payload);
  h.controller = createAuthController({
    invoke: (command, args) => {
      h.calls.push({ command, args });
      try {
        const queue = h.queues.get(command);
        if (queue?.length) {
          const reply = queue.shift();
          // Preserve an injected promise's actual identity, so the microtask
          // boundary regressions exercise the precise awaited rejection edge.
          return Promise.resolve(typeof reply === "function" ? reply(args) : reply);
        }
        if (command === "session_status") return Promise.resolve(h.snapshot);
        if (command === "list_nodes") return Promise.resolve(h.bound(h.nodes.map((node) => ({ ...node }))));
        if (command === "local_status") return Promise.resolve(h.bound({ ...h.local }));
        if (command === "service_status") return Promise.resolve(h.bound(h.service));
        if (command === "diagnostics") return Promise.resolve(h.bound("inert diagnostics"));
        if (["service_action", "approve_device", "init_this_computer"].includes(command)) return Promise.resolve(h.bound(null));
        if (command === "cancel_init") return Promise.resolve(undefined);
        if (command === "reconcile_desktop_helper") return Promise.resolve({ status: "unchanged" });
        throw new Error(`Unexpected inert command ${command}`);
      } catch (error) {
        return Promise.reject(error);
      }
    },
    listen: async (event, receive) => {
      h.subscriptions.push(event);
      h.events.set(event, receive);
      if (h.subscriptionGates.has(event)) await h.subscriptionGates.get(event);
      return () => h.events.delete(event);
    },
    delay: async (milliseconds) => {
      const wait = deferred();
      h.delays.push({ milliseconds, ...wait });
      await wait.promise;
    },
    operationId: () => id(++h.serial),
    render: () => h.renders.push(JSON.parse(JSON.stringify(h.controller.state))),
  });
  h.start = async () => { await h.controller.start(); await flush(); return h; };
  h.logout = async (epoch = "2", revision = "3") => {
    h.enqueue("sign_out", (args) => {
      h.snapshot = view(epoch, revision);
      return ack(args, h.snapshot);
    });
    await h.controller.signOut();
  };
  h.loginB = async (epoch = "4", revision = "6") => {
    h.enqueue("begin_login", (args) => {
      h.snapshot = view(epoch, revision, "b@example.invalid", {
        cancel_target: { expected_auth_epoch: args.expectedAuthEpoch, operation_id: args.operationId },
      });
      h.nodes = [{ id: "B", name: "B", online: true }];
      return ack(args, h.snapshot, { reserved_auth_epoch: String(BigInt(args.expectedAuthEpoch) + 1n) });
    });
    await h.controller.beginLogin();
  };
  return h;
}

test("both listeners precede bootstrap and enabled auth, helper still runs exactly once", async () => {
  const h = fixture(), first = deferred(), second = deferred();
  h.subscriptionGates.set("citadel:auth", first.promise);
  h.subscriptionGates.set("citadel:init", second.promise);
  const started = h.controller.start();
  await h.controller.beginLogin();
  assert.equal(h.count("session_status"), 0);
  assert.equal(h.count("begin_login"), 0);
  assert.equal(h.count("reconcile_desktop_helper"), 1);
  first.resolve(); await flush();
  assert.deepEqual(h.subscriptions, ["citadel:auth", "citadel:init"]);
  assert.equal(h.count("session_status"), 0);
  second.resolve(); await started;
  assert.equal(h.count("session_status"), 1);
  assert.equal(h.controller.state.ready, true);
  await h.controller.start();
  assert.equal(h.count("reconcile_desktop_helper"), 1);
});

test("late initial status cannot replace a newer independently reconciled context", async () => {
  const h = fixture(), initial = deferred();
  h.enqueue("session_status", initial.promise);
  const started = h.controller.start(); await flush();
  h.snapshot = view("3", "5", "b@example.invalid");
  h.nodes = [{ id: "B" }];
  h.emit("citadel:auth", ack({ operationId: id(90), expectedAuthEpoch: "1" }, h.snapshot));
  await flush();
  initial.resolve(view("1", "1", "a@example.invalid"));
  await started; await flush();
  assert.equal(h.controller.state.auth.email, "b@example.invalid");
  assert.deepEqual(h.controller.state.nodes, [{ id: "B" }]);
});

test("cancel while begin awaits uses original epoch and UUID; stale ack/finally cannot clear newer busy", async () => {
  const h = await fixture().start(), begin = deferred(), next = deferred();
  h.enqueue("begin_login", begin.promise);
  const starting = h.controller.beginLogin();
  const original = h.last("begin_login").args;
  assert.equal(h.controller.state.canCancelLogin, true);
  h.enqueue("cancel_login", (args) => {
    assert.equal(args.expectedAuthEpoch, "1");
    assert.equal(args.targetOperationId, original.operationId);
    assert.notEqual(args.operationId, original.operationId);
    h.snapshot = view("3", "4");
    return ack(args, h.snapshot);
  });
  await h.controller.cancelLogin();
  assert.equal(h.controller.state.busy, false);
  h.enqueue("begin_login", next.promise);
  const replacing = h.controller.beginLogin();
  const replacement = h.last("begin_login").args;
  begin.resolve(ack(original, pending("2", "3", "1", original.operationId)));
  await starting;
  assert.equal(h.controller.state.busy, true);
  assert.equal(h.controller.state.canCancelLogin, true);
  h.snapshot = pending("4", "5", "3", replacement.operationId);
  next.resolve(ack(replacement, h.snapshot));
  await replacing;
  assert.equal(h.controller.state.busy, false);
  assert.equal(h.controller.state.ready, true);
  assert.equal(h.controller.state.loginStarted, true);
});

test("early auth event is notification only until begin acknowledgement and fresh status", async () => {
  const h = await fixture().start(), begin = deferred();
  h.enqueue("begin_login", begin.promise);
  const starting = h.controller.beginLogin(), args = h.last("begin_login").args;
  const installed = view("3", "5", "b@example.invalid", {
    cancel_target: { expected_auth_epoch: "1", operation_id: args.operationId },
  });
  const before = h.count("session_status");
  h.emit("citadel:auth", ack(args, installed));
  await flush();
  assert.equal(h.count("session_status"), before);
  assert.equal(h.controller.state.auth.signed_in, false);
  assert.equal(h.controller.state.busy, true);
  h.snapshot = installed;
  begin.resolve(ack(args, pending("2", "3", "1", args.operationId)));
  await starting; await flush();
  assert.equal(h.controller.state.auth.email, "b@example.invalid");
  assert.equal(h.controller.state.busy, false);
  assert.equal(h.controller.state.ready, true);
});

test("old auth event after logout and replacement cannot publish or trigger status", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start();
  await h.logout(); await h.loginB();
  const before = h.count("session_status");
  h.emit("citadel:auth", ack({ operationId: id(80), expectedAuthEpoch: "1" }, view("1", "2", "a@example.invalid"), { error: "old A failure" }));
  await flush();
  assert.equal(h.count("session_status"), before);
  assert.equal(h.controller.state.auth.email, "b@example.invalid");
  assert.notEqual(h.controller.state.notice, "old A failure");
});

test("late A inventory cannot install after B; B inventory failure never retains A", async () => {
  const h = fixture(view("1", "1", "a@example.invalid")), old = deferred();
  h.enqueue("list_nodes", old.promise);
  const started = h.controller.start(); await flush();
  assert.equal(h.controller.state.auth.email, "a@example.invalid");
  await h.logout();
  h.enqueue("list_nodes", () => { throw "B inventory unavailable"; });
  h.enqueue("local_status", () => { throw "B local unavailable"; });
  h.enqueue("service_status", () => { throw "B service unavailable"; });
  await h.loginB();
  old.resolve(h.bound([{ id: "A-private" }], view("1", "1", "a@example.invalid")));
  await started; await flush();
  assert.equal(h.controller.state.auth.email, "b@example.invalid");
  assert.deepEqual(h.controller.state.nodes, []);
  assert.equal(h.controller.state.local, null);
  assert.equal(h.controller.state.service, "");
});

test("reversed refreshes and late refresh rejection cannot roll back or overwrite newer notice", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start(), old = deferred();
  h.enqueue("session_status", old.promise);
  const first = h.controller.refresh();
  h.snapshot = view("1", "2", "updated@example.invalid");
  h.nodes = [{ id: "new inventory" }];
  await h.controller.refresh();
  old.resolve(view("1", "1", "a@example.invalid")); await first;
  assert.equal(h.controller.state.auth.email, "updated@example.invalid");
  assert.deepEqual(h.controller.state.nodes, [{ id: "new inventory" }]);
  const rejected = deferred();
  h.enqueue("session_status", rejected.promise);
  const stale = h.controller.refresh();
  await h.controller.refresh();
  h.controller.state.notice = "newer current notice";
  rejected.reject("old refresh failure"); await stale;
  assert.equal(h.controller.state.notice, "newer current notice");
});

for (const origin of ["refresh", "auth event"]) {
  test(`${origin} error cannot cross the child-to-caller microtask gap into a newer refresh`, async () => {
    const h = await fixture(view("1", "1", "a@example.invalid")).start();
    const oldStatus = deferred(), newStatus = deferred();
    h.enqueue("session_status", oldStatus.promise);
    let oldCompletion;
    if (origin === "refresh") oldCompletion = h.controller.refresh();
    else h.emit("citadel:auth", ack({ operationId: id(90), expectedAuthEpoch: "1" }, h.snapshot));
    h.enqueue("session_status", newStatus.promise);
    let newCompletion;
    // The injected promise is returned without an async wrapper. Rejecting it
    // queues the OLD refreshFrom catch first. This queued callback then starts
    // a NEW same-epoch refresh before the OLD caller's await continuation.
    oldStatus.reject("old child failure");
    queueMicrotask(() => {
      newCompletion = h.controller.refresh();
      h.controller.state.notice = "new refresh owns this notice";
    });
    await flush();
    assert.equal(h.controller.state.notice, "new refresh owns this notice");
    assert.equal(h.count("session_status"), 3);
    newStatus.resolve(view("1", "2", "a@example.invalid"));
    if (oldCompletion) await oldCompletion;
    await newCompletion; await flush();
    assert.equal(h.controller.state.notice, "new refresh owns this notice");
  });
}

test("serviceAction finally retains its owner when a newer same-epoch refresh starts", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start();
  const oldStatus = deferred(), newStatus = deferred();
  h.enqueue("session_status", oldStatus.promise);
  const action = h.controller.serviceAction("start");
  await flush();
  assert.equal(h.controller.state.busy, true);
  assert.equal(h.count("service_action"), 1);
  h.enqueue("session_status", newStatus.promise);
  let newer;
  oldStatus.reject("old action refresh failure");
  queueMicrotask(() => {
    newer = h.controller.refresh();
    h.controller.state.notice = "new refresh notice";
  });
  await action; await flush();
  // Mutating the shared action parent.refresh would make this finally stale
  // solely because a new refresh began, leaving busy permanently true.
  assert.equal(h.controller.state.busy, false);
  assert.equal(h.controller.state.notice, "new refresh notice");
  newStatus.resolve(view("1", "2", "a@example.invalid"));
  await newer;
  assert.equal(h.controller.state.busy, false);
});

test("BoundResult newer revision does not invent an AuthView or a replacement account", async () => {
  const h = fixture(view("1", "1", "old-display@example.invalid"));
  h.enqueue("list_nodes", h.bound([{ id: "same-lineage" }], view("1", "2", "new-display@example.invalid")));
  await h.start();
  assert.equal(h.controller.state.auth.email, "old-display@example.invalid");
  assert.deepEqual(h.controller.state.nodes, [{ id: "same-lineage" }]);
  h.controller.bindInput("nodeName")("preserved input");
  h.snapshot = view("1", "2", "new-display@example.invalid");
  h.nodes = [{ id: "same-lineage" }];
  await h.controller.refresh();
  assert.equal(h.controller.state.auth.email, "new-display@example.invalid");
  assert.equal(h.controller.state.nodeName, "preserved input");
  assert.equal(h.controller.state.notice, "");
});

test("late diagnostics action and finally cannot clear a newer login owner", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start(), report = deferred(), login = deferred();
  h.enqueue("diagnostics", report.promise);
  const action = h.controller.diagnostics();
  await h.logout();
  h.enqueue("begin_login", login.promise);
  const starting = h.controller.beginLogin(), args = h.last("begin_login").args;
  report.resolve(h.bound("A-private diagnostics", view("1", "1", "a@example.invalid")));
  await action;
  assert.equal(h.controller.state.busy, true);
  assert.equal(h.controller.state.diagnostics, "");
  h.snapshot = pending("3", "4", "2", args.operationId);
  login.resolve(ack(args, h.snapshot)); await starting;
  assert.equal(h.controller.state.busy, false);
});

test("late service action cannot issue a follow-up operation with replacement authority", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start(), service = deferred();
  h.enqueue("service_action", service.promise);
  const running = h.controller.serviceAction("start");
  assert.equal(h.last("service_action").args.expectedAuthEpoch, "1");
  await h.logout(); await h.loginB();
  const before = h.calls.length;
  service.resolve(h.bound(null, view("1", "1", "a@example.invalid")));
  await running;
  assert.equal(h.calls.length, before);
  assert.equal(h.controller.state.busy, false);
});

test("pairing poll stops before its next call after auth invalidation", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start();
  h.controller.bindInput("pairCode")("ABCD-1234");
  const pairing = h.controller.pairBox(); await flush();
  assert.equal(h.last("approve_device").args.expectedAuthEpoch, "1");
  assert.equal(h.delays[0].milliseconds, 5_000);
  await h.logout(); await h.loginB();
  const before = h.calls.length;
  h.delays[0].resolve(); await pairing;
  assert.equal(h.calls.length, before);
  assert.equal(h.controller.state.pairCode, "");
  assert.equal(h.controller.state.notice.includes("Box approved"), false);
});

test("setup readiness poll stops after logout without old setup presentation", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start();
  await h.controller.startSetup();
  const args = h.last("init_this_computer").args;
  h.emit("citadel:init", { auth_epoch: "1", setup_operation_id: args.setupOperationId, stage: "ready", message: "A setup ready" });
  await flush();
  assert.equal(h.delays[0].milliseconds, 2_000);
  await h.logout(); await h.loginB();
  const before = h.calls.length;
  h.delays[0].resolve(); await flush();
  assert.equal(h.calls.length, before);
  assert.equal(h.controller.state.setup, "");
});

test("terminal init event before acknowledgement leaves a tombstone; late ack cannot reanimate", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start(), start = deferred();
  h.enqueue("init_this_computer", start.promise);
  const running = h.controller.startSetup(), args = h.last("init_this_computer").args;
  assert.equal(h.controller.state.setupRunning, true);
  h.emit("citadel:init", { auth_epoch: "1", setup_operation_id: args.setupOperationId, stage: "cancelled", message: "Node setup cancelled" });
  await flush();
  assert.equal(h.controller.state.setupRunning, false);
  start.resolve(h.bound(null)); await running;
  assert.equal(h.controller.state.setupRunning, false);
  assert.equal(h.controller.state.setup, "Node setup cancelled");
  assert.equal(h.controller.state.busy, false);
});

test("ambiguous setup error retains exact cancel-only ownership across logout", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start();
  h.enqueue("init_this_computer", () => { throw "ambiguous IPC failure"; });
  await h.controller.startSetup();
  const original = h.last("init_this_computer").args;
  assert.equal(h.controller.state.setupRunning, true);
  const cancel = h.controller.bindSetupCancellation();
  await h.controller.startSetup();
  assert.equal(h.count("init_this_computer"), 1);
  await h.logout();
  assert.equal(h.controller.state.setupRunning, true);
  assert.equal(h.controller.state.setup, "");
  assert.equal(h.controller.state.diagnostics, "");
  await cancel();
  assert.deepEqual(h.last("cancel_init").args, { originatingAuthEpoch: "1", setupOperationId: original.setupOperationId });
  assert.equal(h.controller.state.setupRunning, true);
  assert.equal(h.controller.state.setupCancellationRequested, true);
  h.emit("citadel:init", { auth_epoch: "1", setup_operation_id: original.setupOperationId, stage: "cancelled", message: "former A message" });
  await flush();
  assert.equal(h.controller.state.setupRunning, false);
  assert.equal(h.controller.state.setup, "");
  assert.equal(h.controller.state.notice.includes("former A"), false);
});

test("stale setup events and rendered cancellation cannot select a newer setup", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start();
  await h.controller.startSetup();
  const first = h.last("init_this_computer").args, cancel = h.controller.bindSetupCancellation();
  h.emit("citadel:init", { auth_epoch: "2", setup_operation_id: first.setupOperationId, stage: "error", message: "foreign epoch" });
  h.emit("citadel:init", { auth_epoch: "1", setup_operation_id: id(99), stage: "error", message: "foreign operation" });
  await flush();
  assert.equal(h.controller.state.setupRunning, true);
  assert.equal(h.controller.state.notice, "");
  h.emit("citadel:init", { auth_epoch: "1", setup_operation_id: first.setupOperationId, stage: "cancelled", message: "done" });
  await flush(); await h.controller.startSetup();
  const second = h.last("init_this_computer").args;
  assert.notEqual(first.setupOperationId, second.setupOperationId);
  const before = h.count("cancel_init");
  await cancel();
  h.emit("citadel:init", { auth_epoch: "1", setup_operation_id: first.setupOperationId, stage: "error", message: "late first" });
  await flush();
  assert.equal(h.count("cancel_init"), before);
  assert.equal(h.controller.state.setupRunning, true);
  assert.equal(h.controller.state.notice, "");
});

test("late cancel acknowledgement cannot mutate a new setup owner", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start(), cancelled = deferred();
  await h.controller.startSetup();
  const first = h.last("init_this_computer").args;
  h.enqueue("cancel_init", cancelled.promise);
  const cancellation = h.controller.cancelSetup();
  h.emit("citadel:init", { auth_epoch: "1", setup_operation_id: first.setupOperationId, stage: "cancelled", message: "done" });
  await flush(); await h.controller.startSetup();
  const second = h.last("init_this_computer").args;
  cancelled.resolve(); await cancellation;
  assert.notEqual(first.setupOperationId, second.setupOperationId);
  assert.equal(h.controller.state.setupRunning, true);
  assert.equal(h.controller.state.setupCancellationRequested, false);
  assert.equal(h.controller.state.setup, "Registering this computer");
});

test("MFA cancellation uses original receipt, not current pending epoch", async () => {
  const target = { expected_auth_epoch: "2", operation_id: id(70) };
  const h = await fixture(view("3", "5", null, {
    mfa_pending: true, factors: [{ id: "factor", friendly_name: null }], cancel_target: target,
  })).start();
  h.enqueue("cancel_login", (args) => {
    assert.equal(args.expectedAuthEpoch, "2");
    assert.equal(args.targetOperationId, id(70));
    h.snapshot = view("4", "6");
    return ack(args, h.snapshot);
  });
  await h.controller.cancelLogin();
  assert.equal(h.controller.state.auth.mfa_pending, false);
  assert.equal(h.controller.state.busy, false);
  assert.equal(h.controller.state.canCancelLogin, false);
});

test("MFA successful installation hands its own action to new epoch; stale MFA cannot clear B", async () => {
  const h = await fixture(view("2", "4", null, {
    mfa_pending: true, factors: [{ id: "factor", friendly_name: null }],
    cancel_target: { expected_auth_epoch: "1", operation_id: id(70) },
  })).start();
  h.controller.bindInput("mfaCode")("123456");
  h.enqueue("verify_mfa", (args) => {
    assert.equal(args.factorId, "factor");
    assert.equal(args.code, "123456");
    assert.equal(args.expectedAuthEpoch, "2");
    h.snapshot = view("3", "6", "installed@example.invalid", {
      cancel_target: { expected_auth_epoch: "1", operation_id: id(70) },
    });
    return ack(args, h.snapshot, { reserved_auth_epoch: null });
  });
  await h.controller.verifyMfa();
  assert.equal(h.controller.state.auth.email, "installed@example.invalid");
  assert.equal(h.controller.state.mfaCode, "");
  assert.equal(h.controller.state.busy, false);
  assert.equal(h.controller.state.ready, true);
});

test("MFA remains cancellable while its invocation awaits; late MFA cannot finish cancellation", async () => {
  const original = { expected_auth_epoch: "1", operation_id: id(70) };
  const h = await fixture(view("2", "4", null, {
    mfa_pending: true, factors: [{ id: "factor", friendly_name: null }], cancel_target: original,
  })).start(), verify = deferred();
  h.controller.bindInput("mfaCode")("123456");
  h.enqueue("verify_mfa", verify.promise);
  const verifying = h.controller.verifyMfa(), args = h.last("verify_mfa").args;
  assert.equal(h.controller.state.canCancelLogin, true);
  assert.equal(h.controller.state.mfaCode, "");
  h.enqueue("cancel_login", (cancel) => {
    assert.equal(cancel.expectedAuthEpoch, "1");
    assert.equal(cancel.targetOperationId, id(70));
    h.snapshot = view("3", "6");
    return ack(cancel, h.snapshot);
  });
  await h.controller.cancelLogin();
  verify.resolve(ack(args, view("3", "5", "late@example.invalid", { cancel_target: original }), { reserved_auth_epoch: null }));
  await verifying;
  assert.equal(h.controller.state.auth.signed_in, false);
  assert.equal(h.controller.state.busy, false);
  assert.equal(h.controller.state.ready, true);
});

test("current event errors require exact final stamp and known operation after status", async () => {
  const h = await fixture().start();
  h.enqueue("begin_login", (args) => {
    h.snapshot = pending("2", "3", "1", args.operationId);
    return ack(args, h.snapshot);
  });
  await h.controller.beginLogin();
  const args = h.last("begin_login").args;
  h.snapshot = view("2", "5");
  h.emit("citadel:auth", ack(args, h.snapshot, { error: "current attempt failed" }));
  await flush();
  assert.equal(h.controller.state.notice, "current attempt failed");
  h.emit("citadel:auth", ack({ operationId: id(88), expectedAuthEpoch: "1" }, h.snapshot, { error: "foreign operation error" }));
  await flush();
  assert.equal(h.controller.state.notice, "current attempt failed");
});

test("late automatic-helper result cannot place A's startup notice in B", async () => {
  const h = fixture(view("1", "1", "a@example.invalid")), helper = deferred();
  h.enqueue("reconcile_desktop_helper", helper.promise);
  await h.start(); await h.logout(); await h.loginB();
  helper.resolve({ status: "restart_failed" }); await flush();
  assert.equal(h.count("reconcile_desktop_helper"), 1);
  assert.equal(h.controller.state.notice.includes("could not restart"), false);
});

test("malformed, lossy, regressing and equal-inconsistent auth stamps are never adopted", async () => {
  const cases = [
    view(2, "5", "bad@example.invalid"), view("02", "5", "bad@example.invalid"),
    view("0", "5", "bad@example.invalid"), view("-1", "5", "bad@example.invalid"),
    view("2.0", "5", "bad@example.invalid"), view(String(MAX + 1n), String(MAX + 1n), "bad@example.invalid"),
    view("2", 5, "bad@example.invalid"), view("2", "05", "bad@example.invalid"),
    view("1", "5", "bad@example.invalid"), view("2", "3", "bad@example.invalid"),
    view("3", "4", "bad@example.invalid"), view("2", "4", "different-at-equal-stamp@example.invalid"),
    view("2", "5"),
    view(String(MAX), "5"), view("2", String(MAX)),
    { ...terminal(), signed_in: true, email: "not-sanitized@example.invalid" },
  ];
  for (const invalid of cases) {
    const h = await fixture(view("2", "4", "current@example.invalid")).start();
    h.snapshot = invalid;
    await h.controller.refresh();
    assert.equal(h.controller.state.auth.email, "current@example.invalid");
    assert.equal(h.controller.state.unavailable, false);
    assert.equal(h.controller.state.notice, "Citadel returned invalid or out-of-order sign-in state.");
  }
});

test("terminal status latches unavailable, clears account data, and never retries adoption", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start();
  h.controller.bindInput("nodeName")("private A name");
  await h.controller.diagnostics();
  h.snapshot = terminal(); await h.controller.refresh();
  assert.equal(h.controller.state.unavailable, true);
  assert.equal(h.controller.state.auth.signed_in, false);
  assert.deepEqual(h.controller.state.nodes, []);
  assert.equal(h.controller.state.diagnostics, "");
  assert.equal(h.controller.state.nodeName, "");
  const before = h.calls.length;
  h.snapshot = view("1", "1", "replacement@example.invalid");
  await h.controller.refresh(); await h.controller.beginLogin(); await h.controller.signOut();
  assert.equal(h.calls.length, before);
  assert.equal(h.controller.state.unavailable, true);
});

test("local intent, refresh and action exhaustion fail closed in actual controller methods", async () => {
  for (const [field, invoke] of [
    ["intentRevision", (controller) => controller.beginLogin()],
    ["refreshRevision", (controller) => controller.refresh()],
    ["actionRevision", (controller) => controller.diagnostics()],
  ]) {
    const h = await fixture(field === "intentRevision" ? view() : view("1", "1", "a@example.invalid")).start();
    // Erasable TS-private field injection is confined to this inert test source;
    // production has no test switch, seed/config input, or counter-reset API.
    h.controller[field] = MAX - 1n;
    const before = h.calls.length;
    await invoke(h.controller);
    assert.equal(h.calls.length, before);
    assert.equal(h.controller.state.unavailable, true);
    assert.equal(h.controller.state.busy, false);
    assert.equal(h.controller.state.ready, false);
  }
});

test("last usable local revisions are admitted once, never wrapped or reset", async () => {
  const h = await fixture().start();
  h.controller.intentRevision = MAX - 2n;
  h.controller.actionRevision = MAX - 2n;
  h.enqueue("begin_login", (args) => {
    h.snapshot = pending("2", "3", "1", args.operationId);
    return ack(args, h.snapshot);
  });
  await h.controller.beginLogin();
  assert.equal(h.count("begin_login"), 1);
  assert.equal(h.controller.intentRevision, MAX - 1n);
  assert.equal(h.controller.actionRevision, MAX - 1n);
  assert.equal(h.controller.state.unavailable, false);
  await h.controller.beginLogin();
  assert.equal(h.count("begin_login"), 1);
  assert.equal(h.controller.state.unavailable, true);
});

test("a node result with a foreign epoch cannot publish diagnostics or start a follow-up call", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start();
  h.enqueue("diagnostics", h.bound("foreign diagnostics", view("2", "3", "b@example.invalid")));
  await h.controller.diagnostics();
  assert.equal(h.controller.state.diagnostics, "");
  h.enqueue("service_action", h.bound(null, view("2", "3", "b@example.invalid")));
  const before = h.count("session_status");
  await h.controller.serviceAction("start");
  assert.equal(h.count("session_status"), before);
  assert.equal(h.controller.state.busy, false);
});

test("delayed rendered actions and input callbacks cannot borrow replacement context", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start();
  const oldInput = h.controller.bindInput("nodeName");
  const oldAction = h.controller.bindAction(() => h.controller.diagnostics());
  await h.logout(); await h.loginB();
  const before = h.count("diagnostics");
  oldInput("A-private input"); await oldAction();
  assert.equal(h.controller.state.nodeName, "");
  assert.equal(h.count("diagnostics"), before);
});

test("malformed control acknowledgement reconciles status; it cannot invent a session", async () => {
  const h = await fixture().start();
  h.enqueue("begin_login", (args) => ack({ ...args, operationId: id(90) }, view("3", "5", "wrong@example.invalid")));
  await h.controller.beginLogin();
  assert.equal(h.controller.state.auth.signed_in, false);
  assert.equal(h.controller.state.ready, true);
  assert.equal(h.controller.state.busy, false);
  assert.equal(h.controller.state.notice, "Could not complete the sign-in change. Check sign-in status before trying again.");
});

test("foreign acknowledgement identity cannot publish an error even at the exact current stamp", async () => {
  for (const wrong of ["operation", "origin"]) {
    const h = await fixture().start();
    h.enqueue("begin_login", (args) => ack({
      ...args, operationId: wrong === "operation" ? id(90) : args.operationId,
      expectedAuthEpoch: wrong === "origin" ? "2" : args.expectedAuthEpoch,
    }, h.snapshot, { error: "foreign acknowledgement error" }));
    await h.controller.beginLogin();
    assert.equal(h.controller.state.notice, "Could not complete the sign-in change. Check sign-in status before trying again.");
    assert.equal(h.controller.state.busy, false);
    assert.equal(h.controller.state.ready, true);
  }
});

test("failed acknowledgement reconciliation keeps cancel recovery available without reusing busy", async () => {
  const h = await fixture().start();
  h.enqueue("begin_login", () => { throw "ambiguous start"; });
  h.enqueue("session_status", () => { throw "status unavailable"; });
  await h.controller.beginLogin();
  const begin = h.last("begin_login").args;
  assert.equal(h.controller.state.ready, false);
  assert.equal(h.controller.state.busy, false);
  assert.equal(h.controller.state.canCancelLogin, true);
  h.enqueue("cancel_login", (args) => {
    assert.equal(args.expectedAuthEpoch, "1");
    assert.equal(args.targetOperationId, begin.operationId);
    h.snapshot = view("3", "5");
    return ack(args, h.snapshot);
  });
  await h.controller.cancelLogin();
  assert.equal(h.controller.state.ready, true);
  assert.equal(h.controller.state.busy, false);
});

test("terminal auth still permits cleanup of only the previously owned setup UUID", async () => {
  const h = await fixture(view("1", "1", "a@example.invalid")).start();
  await h.controller.startSetup();
  const original = h.last("init_this_computer").args, cancel = h.controller.bindSetupCancellation();
  h.snapshot = terminal(); await h.controller.refresh();
  assert.equal(h.controller.state.setupRunning, true);
  await cancel();
  assert.deepEqual(h.last("cancel_init").args, { originatingAuthEpoch: "1", setupOperationId: original.setupOperationId });
  assert.equal(h.controller.state.setupRunning, true);
  assert.equal(h.controller.state.auth.signed_in, false);
});

// Terminal authentication cleanup is an explicit, independently owned lane.
// These exercise the production controller, not a detached protocol model.
async function locallyUnavailable(field = "actionRevision", initial = view("1", "100", "a@example.invalid")) {
  const h = await fixture(initial).start();
  h.controller[field] = MAX - 1n;
  if (field === "intentRevision") await h.controller.beginLogin();
  else if (field === "refreshRevision") await h.controller.refresh();
  else await h.controller.diagnostics();
  assert.equal(h.controller.state.unavailable, true);
  return h;
}

test("accepted terminal status permits exact MAX cleanup but never adopts its acknowledgement", async () => {
  const h = await fixture(terminal()).start();
  const accepted = h.controller.accepted;
  const counters = [h.controller.intentRevision, h.controller.refreshRevision, h.controller.actionRevision];
  const before = h.calls.length;
  h.enqueue("sign_out", (args) => {
    assert.equal(args.expectedAuthEpoch, String(MAX));
    return ack(args, terminal(), { reserved_auth_epoch: null });
  });
  await h.controller.bindAuthenticationCleanup()();
  assert.deepEqual(h.calls.slice(before).map((call) => call.command), ["sign_out"]);
  assert.equal(h.controller.accepted, accepted);
  assert.deepEqual([h.controller.intentRevision, h.controller.refreshRevision, h.controller.actionRevision], counters);
  assert.equal(h.controller.state.unavailable, true);
  assert.equal(h.controller.state.ready, false);
  assert.equal(h.controller.state.auth.signed_in, false);
  assert.equal(h.controller.state.authCleanupPending, false);
  assert.match(h.controller.state.authCleanupNotice, /^Cleanup request acknowledged\./);
  assert.match(h.controller.state.notice, /cleanup could not be confirmed/);
  assert.equal(/restart/i.test(h.controller.state.notice), false);
});

test("a rendered callback without cleanup authority cannot borrow a later accepted terminal epoch", async () => {
  const h = await fixture(view("1", "100", "a@example.invalid")).start();
  const earlier = h.controller.bindAuthenticationCleanup();
  await h.controller.cleanupAuthentication();
  assert.equal(h.count("sign_out"), 0);
  h.snapshot = terminal(); await h.controller.refresh();
  await earlier();
  assert.equal(h.count("sign_out"), 0);
  h.enqueue("sign_out", (args) => ack(args, terminal(), { reserved_auth_epoch: null }));
  await h.controller.bindAuthenticationCleanup()();
  assert.equal(h.count("sign_out"), 1);
  assert.equal(h.last("sign_out").args.expectedAuthEpoch, String(MAX));
});

for (const field of ["intentRevision", "refreshRevision", "actionRevision"]) {
  test(`cleanup after ${field} exhaustion keeps the original ordinary epoch and revisions`, async () => {
    const initial = view("1", "100", field === "intentRevision" ? null : "a@example.invalid");
    const h = await locallyUnavailable(field, initial);
    const accepted = h.controller.accepted;
    const counters = [h.controller.intentRevision, h.controller.refreshRevision, h.controller.actionRevision];
    const before = h.calls.length;
    h.enqueue("sign_out", (args) => {
      assert.equal(args.expectedAuthEpoch, "1");
      return ack(args, view("2", "102"));
    });
    await h.controller.cleanupAuthentication();
    assert.deepEqual(h.calls.slice(before).map((call) => call.command), ["sign_out"]);
    assert.equal(h.controller.accepted, accepted);
    assert.deepEqual([h.controller.intentRevision, h.controller.refreshRevision, h.controller.actionRevision], counters);
    assert.match(h.controller.state.authCleanupNotice, /^Cleanup request acknowledged\./);
    assert.equal(h.controller.state.unavailable, true);
    assert.equal(h.controller.state.ready, false);
  });
}

test("terminal cleanup accepts only the frozen reserved and overflow terminal shapes without clearing caution", async () => {
  for (const reserved of ["2", null]) {
    const h = await locallyUnavailable();
    h.enqueue("sign_out", (args) => ack(args, terminal(), { reserved_auth_epoch: reserved }));
    await h.controller.cleanupAuthentication();
    assert.match(h.controller.state.authCleanupNotice, /^Cleanup request acknowledged\./);
    assert.match(h.controller.state.notice, /cleanup could not be confirmed/);
    assert.equal(h.controller.accepted.auth_epoch, "1");
    assert.equal(h.controller.state.unavailable, true);
    assert.equal(h.controller.state.ready, false);
  }
});

test("missing accepted authority and failed subscriptions never manufacture cleanup authority", async () => {
  const h = fixture();
  h.enqueue("session_status", () => { throw "bootstrap unavailable"; });
  await h.start();
  h.controller.refreshRevision = MAX - 1n;
  await h.controller.refresh();
  assert.equal(h.controller.state.unavailable, true);
  assert.equal(h.controller.state.authCleanupAvailable, false);
  const before = h.calls.length;
  await h.controller.cleanupAuthentication();
  await h.controller.bindAuthenticationCleanup()();
  assert.equal(h.calls.length, before);

  const failed = fixture();
  failed.subscriptionGates.set("citadel:init", Promise.reject("subscription unavailable"));
  await failed.start();
  const failedBefore = failed.calls.length;
  await failed.controller.cleanupAuthentication();
  assert.equal(failed.calls.length, failedBefore);
  assert.equal(failed.controller.state.authCleanupAvailable, false);
});

test("first disable alone latches exact E and V; repeated disable cannot retarget or invent a missing binding", async () => {
  const h = await locallyUnavailable();
  const binding = h.controller.cleanupBinding;
  const rendered = h.controller.bindAuthenticationCleanup();
  // White-box boundary injection exercises the actual first-disable latch:
  // production has no setter, reset, seed, or late accepted-view adoption path.
  h.controller.accepted = view("7", "700", "foreign@example.invalid");
  h.controller.disable();
  assert.equal(h.controller.cleanupBinding, binding);
  assert.deepEqual(binding, { epoch: "1", viewRevision: "100" });
  h.enqueue("sign_out", (args) => {
    assert.equal(args.expectedAuthEpoch, "1");
    return ack(args, view("2", "2"));
  });
  await rendered();
  assert.match(h.controller.state.authCleanupNotice, /cleanup could not be confirmed/);

  const absent = fixture();
  absent.controller.disable();
  absent.controller.accepted = terminal();
  absent.controller.disable();
  await absent.controller.cleanupAuthentication();
  assert.equal(absent.count("sign_out"), 0);
  assert.equal(absent.controller.cleanupBinding, null);
});

test("ordinary cleanup rejects regressing or equal V and accepts only a greater captured revision", async () => {
  for (const revision of ["2", "100", "101"]) {
    const h = await locallyUnavailable();
    h.enqueue("sign_out", (args) => ack(args, view("2", revision)));
    await h.controller.cleanupAuthentication();
    assert.equal(h.controller.state.authCleanupNotice.startsWith("Cleanup request acknowledged."), revision === "101");
    assert.equal(h.controller.accepted.view_revision, "100");
    assert.equal(h.controller.state.unavailable, true);
  }
});

test("malformed, foreign, superseded, nonempty and lossy cleanup outcomes cannot publish success or raw errors", async () => {
  const cases = [
    (args) => ({ ...ack(args, view("2", "102")), operation_id: id(999) }),
    (args) => ({ ...ack(args, view("2", "102")), originating_auth_epoch: "2" }),
    (args) => ack(args, view("2", "102"), { reserved_auth_epoch: null }),
    (args) => ack(args, view("3", "102"), { reserved_auth_epoch: "3" }),
    (args) => ack(args, view("3", "102"), { reserved_auth_epoch: "2" }),
    (args) => ack(args, terminal(), { reserved_auth_epoch: String(MAX) }),
    (args) => ack(args, view("2", "102"), { reserved_auth_epoch: "02" }),
    (args) => ack(args, view("2", "102"), { reserved_auth_epoch: 2 }),
    (args) => ack(args, view("2", "0102")),
    (args) => ack(args, view("2", 102)),
    (args) => ack(args, view("2", "102", "private@example.invalid")),
    (args) => ack(args, pending("2", "102", "1", id(70))),
    (args) => ack(args, { ...view("2", "102"), mfa_pending: true, factors: [{ id: "private-factor", friendly_name: null }] }),
    (args) => ack(args, view(String(MAX), "102"), { reserved_auth_epoch: null }),
    (args) => ack(args, view("2", "102"), { superseded: true }),
    (args) => ack(args, view("2", "102"), { superseded: "false" }),
    (args) => ack(args, view("2", "102"), { error: { private: "not a string" } }),
    (args) => ack(args, view("2", "102"), { error: "private backend credential text" }),
    (args) => ack(args, terminal(), { reserved_auth_epoch: null, error: "private overflow uncertainty" }),
    (args) => ack(args, terminal(), { reserved_auth_epoch: "2", error: "private reserved terminal uncertainty" }),
    (args) => ({ ...ack(args, view("2", "102")), operation_id: undefined }),
    (args) => ({ ...ack(args, view("2", "102")), originating_auth_epoch: undefined }),
    (args) => ({ ...ack(args, view("2", "102")), reserved_auth_epoch: undefined }),
    (args) => ({ ...ack(args, view("2", "102")), view: undefined }),
    (args) => ({ ...ack(args, view("2", "102")), error: undefined }),
    (args) => ({ ...ack(args, view("2", "102")), superseded: undefined }),
    () => null,
  ];
  for (const response of cases) {
    const h = await locallyUnavailable();
    const before = h.calls.length;
    h.enqueue("sign_out", response);
    await h.controller.cleanupAuthentication();
    assert.match(h.controller.state.authCleanupNotice, /cleanup could not be confirmed/);
    assert.equal(JSON.stringify(h.controller.state).includes("private"), false);
    assert.deepEqual(h.calls.slice(before).map((call) => call.command), ["sign_out"]);
    assert.equal(h.controller.accepted.auth_epoch, "1");
    assert.equal(h.controller.state.unavailable, true);
    assert.equal(h.controller.state.ready, false);
  }
});

test("MAX cleanup refuses any reservation and every nonterminal or non-null-error result", async () => {
  for (const change of [
    { reserved_auth_epoch: "1" },
    { reserved_auth_epoch: String(MAX) },
    { view: view("1", "1") },
    { error: "remote unknown issuance remains" },
    { originating_auth_epoch: "1" },
  ]) {
    const h = await fixture(terminal()).start();
    h.enqueue("sign_out", (args) => ack(args, terminal(), { reserved_auth_epoch: null, ...change }));
    await h.controller.cleanupAuthentication();
    assert.match(h.controller.state.authCleanupNotice, /cleanup could not be confirmed/);
    assert.equal(h.controller.state.unavailable, true);
  }
});

test("cleanup has one finite in-flight owner and retries only explicitly with a fresh UUID and same binding", async () => {
  const h = await locallyUnavailable(), first = deferred();
  const rendered = h.controller.bindAuthenticationCleanup();
  h.enqueue("sign_out", first.promise);
  const running = rendered(), args = h.last("sign_out").args;
  assert.equal(h.controller.state.authCleanupPending, true);
  await rendered(); await h.controller.cleanupAuthentication();
  assert.equal(h.count("sign_out"), 1);
  first.reject("private ambiguous remote error"); await running;
  assert.equal(h.controller.state.authCleanupPending, false);
  assert.equal(h.count("sign_out"), 1);
  h.enqueue("sign_out", (next) => {
    assert.equal(next.expectedAuthEpoch, args.expectedAuthEpoch);
    assert.notEqual(next.operationId, args.operationId);
    return ack(next, view("2", "102"));
  });
  await rendered();
  assert.equal(h.count("sign_out"), 2);
  assert.equal(h.controller.state.unavailable, true);
});

test("cleanup rejection and invalid UUID never retry as MAX, reconcile status, or leak raw exceptions", async () => {
  const h = await locallyUnavailable();
  h.enqueue("sign_out", () => { throw "private transport detail"; });
  const before = h.calls.length;
  await h.controller.cleanupAuthentication();
  assert.deepEqual(h.calls.slice(before).map((call) => call.command), ["sign_out"]);
  assert.match(h.controller.state.authCleanupNotice, /cleanup could not be confirmed/);
  h.controller.dependencies.operationId = () => "not-a-uuid";
  const after = h.calls.length;
  await h.controller.cleanupAuthentication();
  assert.equal(h.calls.length, after);
  assert.equal(h.controller.state.authCleanupPending, false);
  assert.equal(JSON.stringify(h.controller.state).includes("private"), false);
});

test("cleanup completes all owner writes before paint can admit a newer explicit request", async () => {
  const h = await locallyUnavailable(), first = deferred(), second = deferred();
  const originalRender = h.controller.dependencies.render;
  let secondCompletion;
  h.controller.dependencies.render = () => {
    originalRender();
    if (h.controller.state.authCleanupNotice.startsWith("Cleanup request acknowledged.") && !secondCompletion) {
      // Mark before invoking because admission itself synchronously renders.
      secondCompletion = Promise.resolve();
      h.enqueue("sign_out", second.promise);
      secondCompletion = h.controller.cleanupAuthentication();
    }
  };
  h.enqueue("sign_out", first.promise);
  const firstCompletion = h.controller.cleanupAuthentication();
  first.resolve(ack(h.last("sign_out").args, view("2", "102")));
  await firstCompletion;
  assert.equal(h.count("sign_out"), 2);
  assert.equal(h.controller.state.authCleanupPending, true);
  assert.match(h.controller.state.authCleanupNotice, /request pending/);
  second.resolve(ack(h.last("sign_out").args, view("2", "103")));
  await secondCompletion;
  assert.equal(h.controller.state.authCleanupPending, false);
});

for (const completion of ["acknowledgement", "rejection"]) {
  test(`stale rendered cleanup binding and late ownership-lost ${completion} cannot select another binding`, async () => {
    const h = await locallyUnavailable(), result = deferred();
    const rendered = h.controller.bindAuthenticationCleanup();
    h.enqueue("sign_out", result.promise);
    const running = rendered(), oldArgs = h.last("sign_out").args;
    // Identity injection tests the real continuation fence; no production API
    // can replace this permanently latched object or confer new authority.
    const otherBinding = { epoch: "1", viewRevision: "100" };
    const otherOwner = { binding: otherBinding, operation: id(900) };
    h.controller.cleanupBinding = otherBinding;
    h.controller.cleanupOwner = otherOwner;
    h.controller.state.authCleanupNotice = "new owner notice";
    await rendered();
    assert.equal(h.count("sign_out"), 1);
    if (completion === "acknowledgement") result.resolve(ack(oldArgs, view("2", "102")));
    else result.reject("private former-owner error");
    await running;
    assert.equal(h.controller.cleanupOwner, otherOwner);
    assert.equal(h.controller.state.authCleanupPending, true);
    assert.equal(h.controller.state.authCleanupNotice, "new owner notice");
  });
}

test("late status and auth notification cannot revive or retarget a pending terminal cleanup", async () => {
  const h = await fixture(view("1", "100", "a@example.invalid")).start();
  const status = deferred(), cleanup = deferred();
  h.enqueue("session_status", status.promise);
  const refreshing = h.controller.refresh();
  h.snapshot = terminal(); await h.controller.refresh();
  const binding = h.controller.cleanupBinding;
  h.enqueue("sign_out", cleanup.promise);
  const running = h.controller.cleanupAuthentication(), args = h.last("sign_out").args;
  const before = h.calls.length;
  h.emit("citadel:auth", ack(args, terminal(), { reserved_auth_epoch: null, error: "private old auth error" }));
  status.resolve(view("1", "101", "private@example.invalid"));
  await refreshing; await flush();
  assert.equal(h.calls.length, before);
  assert.equal(h.controller.cleanupBinding, binding);
  assert.equal(h.controller.accepted.auth_epoch, String(MAX));
  assert.equal(h.controller.state.authCleanupPending, true);
  assert.equal(h.controller.state.auth.signed_in, false);
  assert.equal(JSON.stringify(h.controller.state).includes("private"), false);
  cleanup.resolve(ack(args, terminal(), { reserved_auth_epoch: null })); await running;
  assert.equal(h.controller.state.unavailable, true);
});

test("late account work cannot revive authentication while exact setup and auth cleanup stay independent", async () => {
  const h = fixture(view("1", "100", "a@example.invalid")), inventory = deferred(), cleanup = deferred();
  h.enqueue("list_nodes", inventory.promise);
  const started = h.controller.start(); await flush();
  await h.controller.startSetup();
  const setup = h.last("init_this_computer").args;
  h.snapshot = terminal(); await h.controller.refresh();
  h.enqueue("sign_out", cleanup.promise);
  const running = h.controller.cleanupAuthentication(), args = h.last("sign_out").args;
  await h.controller.bindSetupCancellation()();
  assert.deepEqual(h.last("cancel_init").args, { originatingAuthEpoch: "1", setupOperationId: setup.setupOperationId });
  assert.equal(h.controller.state.setupRunning, true);
  h.emit("citadel:init", { auth_epoch: "1", setup_operation_id: setup.setupOperationId, stage: "cancelled", message: "private former setup" });
  h.emit("citadel:auth", ack({ operationId: id(80), expectedAuthEpoch: "1" }, view("1", "101", "private@example.invalid")));
  inventory.resolve(h.bound([{ id: "private former inventory" }], view("1", "100", "a@example.invalid")));
  await started; await flush();
  assert.equal(h.controller.state.authCleanupPending, true);
  assert.equal(h.controller.state.setupRunning, false);
  assert.equal(h.controller.state.setup, "");
  assert.deepEqual(h.controller.state.nodes, []);
  cleanup.resolve(ack(args, terminal(), { reserved_auth_epoch: null })); await running;
  const before = h.calls.length;
  await h.controller.beginLogin(); await h.controller.signOut(); await h.controller.refresh();
  assert.equal(h.calls.length, before);
  assert.equal(h.controller.state.unavailable, true);
  assert.equal(h.controller.state.ready, false);
  assert.equal(h.controller.state.auth.signed_in, false);
  assert.equal(JSON.stringify(h.controller.state).includes("private"), false);
});
