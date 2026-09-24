use crate::auth::{valid_device_code, AuthState};
use reqwest::Method;
use serde::Serialize;
use serde_json::{json, Value};
use std::sync::atomic::{AtomicBool, Ordering};
use tauri::{AppHandle, Emitter, Manager, State};
use tauri_plugin_shell::{process::CommandEvent, ShellExt};

pub struct InitState(pub AtomicBool);

impl Default for InitState {
    fn default() -> Self {
        Self(AtomicBool::new(false))
    }
}

#[derive(Serialize, Clone)]
pub struct InitEvent {
    stage: &'static str,
    message: String,
}

fn emit(app: &AppHandle, stage: &'static str, message: impl Into<String>) {
    let _ = app.emit(
        "citadel:init",
        InitEvent {
            stage,
            message: message.into(),
        },
    );
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
pub async fn local_status(app: AppHandle) -> Result<Value, String> {
    let text = output(&app, &["status", "--json"]).await?;
    serde_json::from_str(&text).map_err(|_| "Citadel returned invalid status".to_string())
}

#[tauri::command]
pub async fn service_status(app: AppHandle) -> Result<String, String> {
    output(&app, &["service", "status"]).await
}

#[tauri::command]
pub async fn service_action(app: AppHandle, action: String) -> Result<(), String> {
    if action != "start" && action != "stop" {
        return Err("Unsupported node action".to_string());
    }
    output(&app, &["service", &action]).await.map(|_| ())
}

#[tauri::command]
pub async fn diagnostics(app: AppHandle) -> Result<String, String> {
    // Explicitly requested by the user. Never uploaded by the app.
    output(&app, &["doctor"]).await
}

#[tauri::command]
pub async fn list_nodes(auth: State<'_, AuthState>) -> Result<Value, String> {
    auth.request(Method::GET, "/api/fabric/nodes", None).await
}

#[tauri::command]
pub async fn approve_device(auth: State<'_, AuthState>, code: String) -> Result<(), String> {
    let code = code.trim().to_ascii_uppercase();
    if !valid_device_code(&code) {
        return Err("Enter the eight-character code shown on the box".to_string());
    }
    auth.request(
        Method::POST,
        "/api/fabric/device-auth/approve",
        Some(json!({ "user_code": code })),
    )
    .await
    .map(|_| ())
}

fn code_from_line(line: &str) -> Option<String> {
    let suffix = line.split_once("Or enter code manually:")?.1.trim();
    valid_device_code(suffix).then(|| suffix.to_string())
}

#[tauri::command]
pub fn init_this_computer(
    app: AppHandle,
    state: State<'_, InitState>,
    node_name: String,
) -> Result<(), String> {
    let name = node_name.trim();
    if name.is_empty() || name.len() > 64 || name.chars().any(char::is_control) {
        return Err("Enter a node name of at most 64 characters".to_string());
    }
    if state
        .0
        .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
        .is_err()
    {
        return Err("Node setup is already running".to_string());
    }
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
            state.0.store(false, Ordering::SeqCst);
            return Err(error);
        }
    };
    let (mut receiver, child) = match command.spawn() {
        Ok(pair) => pair,
        Err(_) => {
            state.0.store(false, Ordering::SeqCst);
            return Err("Could not start the bundled Citadel node helper".to_string());
        }
    };
    let app_for_task = app.clone();
    tauri::async_runtime::spawn(async move {
        let mut child = Some(child);
        emit(&app_for_task, "registering", "Registering this computer");
        let mut approved = false;
        let mut exit_code = None;
        let mut had_error = false;
        while let Some(event) = receiver.recv().await {
            match event {
                CommandEvent::Stdout(bytes) if !approved => {
                    if let Some(code) = code_from_line(&String::from_utf8_lossy(&bytes)) {
                        approved = true;
                        emit(&app_for_task, "approving", "Approving this computer");
                        let auth = app_for_task.state::<AuthState>();
                        if auth
                            .request(
                                Method::POST,
                                "/api/fabric/device-auth/approve",
                                Some(json!({ "user_code": code })),
                            )
                            .await
                            .is_err()
                        {
                            had_error = true;
                            if let Some(process) = child.take() {
                                let _ = process.kill();
                            }
                            emit(
                                &app_for_task,
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
        if !had_error && exit_code == Some(0) {
            emit(
                &app_for_task,
                "ready",
                "Citadel started. Waiting for this computer to appear online",
            );
        } else if !had_error {
            emit(
                &app_for_task,
                "error",
                format!("Node setup stopped ({})", exit_code.unwrap_or(-1)),
            );
        }
        app_for_task
            .state::<InitState>()
            .0
            .store(false, Ordering::SeqCst);
    });
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::code_from_line;

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
}
