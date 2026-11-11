use crate::auth::{valid_device_code, valid_operation_id, AuthOrigin, AuthState, BoundResult};
use reqwest::Method;
use serde::Serialize;
use serde_json::{json, Value};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Mutex;
use std::time::Duration;
use tauri::{AppHandle, Emitter, Manager, State};
use tauri_plugin_shell::{
    process::{CommandChild, CommandEvent},
    ShellExt,
};
use tokio::sync::watch;

const SETUP_TIMEOUT: Duration = Duration::from_secs(180);

pub struct InitState {
    active: AtomicBool,
    owner: Mutex<Option<SetupBinding>>,
    cancelled: AtomicBool,
    child: Mutex<Option<CommandChild>>,
    cancel_signal: watch::Sender<bool>,
}

impl Default for InitState {
    fn default() -> Self {
        Self {
            active: AtomicBool::new(false),
            owner: Mutex::new(None),
            cancelled: AtomicBool::new(false),
            child: Mutex::new(None),
            cancel_signal: watch::channel(false).0,
        }
    }
}

impl InitState {
    pub fn cancel(&self) {
        if !self.active.load(Ordering::SeqCst) {
            return;
        }
        self.cancelled.store(true, Ordering::SeqCst);
        self.cancel_signal.send_replace(true);
        if let Ok(mut child) = self.child.lock() {
            if let Some(process) = child.take() {
                let _ = process.kill();
            }
        }
    }
}

#[derive(Clone)]
struct SetupBinding {
    origin: AuthOrigin,
    operation_id: String,
}

async fn setup_binding(auth: &AuthState, expected: &str, operation_id: &str)
    -> Result<SetupBinding, String>
{
    if !valid_operation_id(operation_id) { return Err("Invalid setup operation".into()); }
    Ok(SetupBinding { origin: auth.capture_origin(expected).await?,
        operation_id: operation_id.into() })
}

async fn approve_setup_code(auth: &AuthState, binding: &SetupBinding, code: &str)
    -> Result<(), String>
{
    if !valid_device_code(code) { return Err("Invalid device code".into()); }
    auth.request_bound(&binding.origin, Method::POST, "/api/fabric/device-auth/approve",
        Some(json!({ "user_code": code }))).await.map(|_| ())
}

fn cancel_owned_setup(state: &InitState, epoch: &str, operation_id: &str)
    -> Result<(), String>
{
    // Hold the ownership selection through dispatch so an old cleanup cannot
    // select a newly admitted setup. This is not process-completion proof.
    let owner = state.owner.lock().map_err(|_| "Node setup state is unavailable")?;
    let exact = owner.as_ref().is_some_and(|current| {
        current.origin.epoch_text() == epoch && current.operation_id == operation_id
    });
    if !exact { return Err("Node setup operation changed".into()); }
    state.cancel();
    Ok(())
}

#[derive(Serialize, Clone)]
pub struct InitEvent {
    auth_epoch: String,
    setup_operation_id: String,
    stage: &'static str,
    message: String,
}

fn emit(app: &AppHandle, binding: &SetupBinding, stage: &'static str, message: impl Into<String>) {
    emit_to(binding, stage, message, |event| { let _ = app.emit("citadel:init", event); });
}

fn emit_to(binding: &SetupBinding, stage: &'static str, message: impl Into<String>,
    publish: impl FnOnce(&InitEvent))
{
    publish(&InitEvent { auth_epoch: binding.origin.epoch_text(),
        setup_operation_id: binding.operation_id.clone(), stage, message: message.into() });
}

fn sidecar(app: &AppHandle) -> Result<tauri_plugin_shell::process::Command, String> {
    app.shell()
        .sidecar("citadel")
        .map(|command| command.arg("--no-auto-update"))
        .map_err(|_| "The bundled Citadel node helper is missing".to_string())
}

async fn output(app: &AppHandle, args: &[&str]) -> Result<String, String> {
    let output = sidecar(app)?
        .args(args)
        .output()
        .await
        .map_err(|_| "Could not run the Citadel node helper".to_string())?;
    if !output.status.success() {
        return Err(format!(
            "Citadel command failed ({})",
            output.status.code().unwrap_or(-1)
        ));
    }
    String::from_utf8(output.stdout).map_err(|_| "Citadel returned invalid text".to_string())
}

#[tauri::command]
pub async fn local_status(app: AppHandle, auth: State<'_, AuthState>,
    expected_auth_epoch: String) -> Result<BoundResult<Value>, String>
{
    let origin = auth.capture_origin(&expected_auth_epoch).await?;
    let text = output(&app, &["status", "--json"]).await?;
    let value = serde_json::from_str(&text).map_err(|_| "Citadel returned invalid status")?;
    auth.finish_bound(&origin, value)
}

#[tauri::command]
pub async fn service_status(app: AppHandle, auth: State<'_, AuthState>,
    expected_auth_epoch: String) -> Result<BoundResult<String>, String>
{
    let origin = auth.capture_origin(&expected_auth_epoch).await?;
    let value = output(&app, &["service", "status"]).await?;
    auth.finish_bound(&origin, value)
}

#[tauri::command]
pub async fn reconcile_desktop_helper(app: AppHandle) -> Result<Value, String> {
    if !cfg!(target_os = "macos") {
        return Ok(json!({ "status": "unsupported" }));
    }
    let text = output(&app, &["service", "reconcile-desktop"]).await?;
    serde_json::from_str(&text).map_err(|_| "Citadel returned invalid service status".to_string())
}

#[tauri::command]
pub async fn service_action(app: AppHandle, auth: State<'_, AuthState>,
    expected_auth_epoch: String, action: String) -> Result<BoundResult<()>, String>
{
    if action != "start" && action != "stop" {
        return Err("Unsupported node action".to_string());
    }
    let origin = auth.capture_origin(&expected_auth_epoch).await?;
    output(&app, &["service", &action]).await?;
    auth.finish_bound(&origin, ())
}

#[tauri::command]
pub async fn diagnostics(app: AppHandle, auth: State<'_, AuthState>,
    expected_auth_epoch: String) -> Result<BoundResult<String>, String>
{
    let origin = auth.capture_origin(&expected_auth_epoch).await?;
    let value = output(&app, &["doctor"]).await?;
    auth.finish_bound(&origin, value)
}

#[tauri::command]
pub async fn list_nodes(auth: State<'_, AuthState>, expected_auth_epoch: String)
    -> Result<BoundResult<Value>, String>
{
    let origin = auth.capture_origin(&expected_auth_epoch).await?;
    auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await
}

#[tauri::command]
pub async fn approve_device(auth: State<'_, AuthState>, expected_auth_epoch: String,
    code: String) -> Result<BoundResult<()>, String>
{
    let code = code.trim().to_ascii_uppercase();
    if !valid_device_code(&code) {
        return Err("Enter the eight-character code shown on the box".to_string());
    }
    let origin = auth.capture_origin(&expected_auth_epoch).await?;
    auth.request_bound(&origin, Method::POST, "/api/fabric/device-auth/approve",
        Some(json!({ "user_code": code }))).await?;
    auth.finish_bound(&origin, ())
}

fn code_from_line(line: &str) -> Option<String> {
    let suffix = line.split_once("Or enter code manually:")?.1.trim();
    valid_device_code(suffix).then(|| suffix.to_string())
}

#[tauri::command]
pub async fn init_this_computer(
    app: AppHandle,
    auth: State<'_, AuthState>,
    state: State<'_, InitState>,
    expected_auth_epoch: String,
    setup_operation_id: String,
    node_name: String,
) -> Result<BoundResult<()>, String> {
    let name = node_name.trim();
    if name.is_empty() || name.len() > 64 || name.chars().any(char::is_control) {
        return Err("Enter a node name of at most 64 characters".to_string());
    }
    let binding = setup_binding(&auth, &expected_auth_epoch, &setup_operation_id).await?;
    {
        let mut owner = state.owner.lock().map_err(|_| "Node setup state is unavailable")?;
        if state.active.compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst).is_err() {
            return Err("Node setup is already running".to_string());
        }
        *owner = Some(binding.clone());
        state.cancelled.store(false, Ordering::SeqCst);
        state.cancel_signal.send_replace(false);
    }
    let mut cancel_rx = state.cancel_signal.subscribe();
    let command = match sidecar(&app) {
        Ok(command) => command.args([
            "init",
            "--service",
            "none",
            "--test=false",
            "--node-name",
            name,
        ]),
        Err(error) => {
            state.active.store(false, Ordering::SeqCst);
            return Err(error);
        }
    };
    let (mut receiver, child) = match command.spawn() {
        Ok(pair) => pair,
        Err(_) => {
            state.active.store(false, Ordering::SeqCst);
            return Err("Could not start the bundled Citadel node helper".to_string());
        }
    };
    match state.child.lock() {
        Ok(mut slot) => *slot = Some(child),
        Err(_) => {
            let _ = child.kill();
            state.active.store(false, Ordering::SeqCst);
            return Err("Node setup state is unavailable".to_string());
        }
    }
    if state.cancelled.load(Ordering::SeqCst) {
        state.cancel();
    }
    let app_for_task = app.clone();
    let task_binding = binding.clone();
    tauri::async_runtime::spawn(async move {
        emit(&app_for_task, &task_binding, "registering", "Registering this computer");
        let mut approved = false;
        let mut exit_code = None;
        let mut had_error = false;
        let timeout = tokio::time::sleep(SETUP_TIMEOUT);
        tokio::pin!(timeout);
        loop {
            let state = app_for_task.state::<InitState>();
            if state.cancelled.load(Ordering::SeqCst) {
                break;
            }
            let event = tokio::select! {
                biased;
                _ = cancel_rx.changed() => break,
                _ = &mut timeout => {
                    had_error = true;
                    emit(&app_for_task, &task_binding, "error", "Node setup timed out. Please try again");
                    break;
                }
                event = receiver.recv() => match event {
                    Some(event) => event,
                    None => break,
                },
            };
            match event {
                CommandEvent::Stdout(bytes) if !approved => {
                    if let Some(code) = code_from_line(&String::from_utf8_lossy(&bytes)) {
                        if state.cancelled.load(Ordering::SeqCst) {
                            break;
                        }
                        approved = true;
                        emit(&app_for_task, &task_binding, "approving", "Approving this computer");
                        let auth = app_for_task.state::<AuthState>();
                        let approval = approve_setup_code(&auth, &task_binding, &code);
                        let result = tokio::select! {
                            biased;
                            _ = cancel_rx.changed() => break,
                            _ = &mut timeout => {
                                had_error = true;
                                emit(&app_for_task, &task_binding, "error", "Node setup timed out. Please try again");
                                break;
                            }
                            result = approval => result,
                        };
                        if state.cancelled.load(Ordering::SeqCst) {
                            break;
                        }
                        if result.is_err() {
                            had_error = true;
                            emit(
                                &app_for_task,
                                &task_binding,
                                "error",
                                "Could not approve this computer. Check your sign in and try again",
                            );
                            break;
                        }
                    }
                }
                CommandEvent::Error(_) => had_error = true,
                CommandEvent::Terminated(result) => exit_code = result.code,
                _ => {}
            }
        }
        let state = app_for_task.state::<InitState>();
        if let Ok(mut slot) = state.child.lock() {
            if let Some(process) = slot.take() {
                if exit_code != Some(0) {
                    let _ = process.kill();
                }
            }
        }
        if state.cancelled.load(Ordering::SeqCst) {
            emit(&app_for_task, &task_binding, "cancelled", "Node setup cancelled");
        } else if !had_error && exit_code == Some(0) {
            emit(
                &app_for_task,
                &task_binding,
                "ready",
                "Citadel started. Waiting for this computer to appear online",
            );
        } else if !had_error {
            emit(
                &app_for_task,
                &task_binding,
                "error",
                format!("Node setup stopped ({})", exit_code.unwrap_or(-1)),
            );
        }
        state.active.store(false, Ordering::SeqCst);
    });
    auth.finish_bound(&binding.origin, ())
}

#[tauri::command]
pub fn cancel_init(state: State<'_, InitState>, originating_auth_epoch: String,
    setup_operation_id: String) -> Result<(), String>
{
    cancel_owned_setup(&state, &originating_auth_epoch, &setup_operation_id)
}

#[cfg(test)]
mod tests {
    use super::{approve_setup_code, cancel_owned_setup, code_from_line, emit_to, setup_binding, InitState};
    use crate::auth::AuthState;
    use std::sync::atomic::Ordering;

    #[test]
    fn parses_only_the_device_auth_line() {
        assert_eq!(
            code_from_line("Or enter code manually:      ABCD-1234"),
            Some("ABCD-1234".into())
        );
        assert_eq!(
            code_from_line("Or enter code manually:      ABCD-1234 and more"),
            None
        );
        assert_eq!(code_from_line("ABCDis-1234"), None);
    }

    #[test]
    fn cancellation_signals_an_active_setup() {
        let state = InitState::default();
        let receiver = state.cancel_signal.subscribe();
        state.active.store(true, Ordering::SeqCst);
        state.cancel();
        assert!(state.cancelled.load(Ordering::SeqCst));
        assert!(receiver.has_changed().unwrap());
        assert!(*receiver.borrow());
    }
    #[test]
    fn delayed_setup_code_uses_captured_origin_not_a_later_session() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let binding = setup_binding(&auth, "1", "11111111-1111-4111-8111-111111111111")
                .await.unwrap();
            io.raw_logout();
            let logout = auth.sign_out("1", "22222222-2222-4222-8222-222222222222").await.unwrap();
            io.queue_login();
            let begun = auth.begin_login(&logout.view.auth_epoch,
                "33333333-3333-4333-8333-333333333333").await.unwrap();
            auth.open_begin(begun, |_| Ok(())).await;
            let replacement = auth.exchange_callback(&format!(
                "citadel://auth/callback?code={}&state={}", "a".repeat(64), "b".repeat(64)))
                .await.unwrap();
            assert!(replacement.view.signed_in);
            let code = code_from_line("Or enter code manually:      ABCD-1234").unwrap();
            assert!(approve_setup_code(&auth, &binding, &code).await.is_err());
            assert!(io.request_count() == 3);
        });
    }
    #[test]
    fn owned_cancel_cannot_select_another_setup_and_does_not_prove_stop() {
        tauri::async_runtime::block_on(async {
            let (auth, _io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let binding = setup_binding(&auth, "1", "11111111-1111-4111-8111-111111111111")
                .await.unwrap();
            let state = InitState::default();
            *state.owner.lock().unwrap() = Some(binding.clone());
            state.active.store(true, Ordering::SeqCst);
            assert!(cancel_owned_setup(&state, "1", "33333333-3333-4333-8333-333333333333").is_err());
            assert!(!state.cancelled.load(Ordering::SeqCst));
            assert!(cancel_owned_setup(&state, "1", &binding.operation_id).is_ok());
            assert!(state.cancelled.load(Ordering::SeqCst));
            assert!(state.active.load(Ordering::SeqCst));
        });
    }
    #[test]
    fn every_actual_init_event_keeps_original_admission_stamp_after_logout() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let operation = "11111111-1111-4111-8111-111111111111";
            let binding = setup_binding(&auth, "1", operation).await.unwrap();
            io.raw_logout();
            auth.sign_out("1", "22222222-2222-4222-8222-222222222222").await.unwrap();
            for stage in ["registering", "approving", "ready", "cancelled", "error"] {
                let mut seen = false;
                emit_to(&binding, stage, "inert stage", |event| {
                    assert!(event.auth_epoch == "1" && event.setup_operation_id == operation);
                    assert!(event.stage == stage && event.message == "inert stage");
                    seen = true;
                });
                assert!(seen);
            }
            // These are stale UI notifications, not evidence of process stop.
            assert!(setup_binding(&auth, "1", operation).await.is_err());
            assert!(auth.finish_bound(&binding.origin, "late local output").is_err());
            assert!(io.request_count() == 1);
        });
    }
}

