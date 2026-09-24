mod auth;
mod node;

use auth::{AuthState, AuthView};
use serde::Serialize;
use std::sync::{Arc, Mutex};
use tauri::{AppHandle, Emitter, Manager, State};
use tauri_plugin_deep_link::DeepLinkExt;
use tauri_plugin_opener::OpenerExt;

#[derive(Clone, Serialize)]
struct AuthEvent {
    view: Option<AuthView>,
    error: Option<String>,
}

fn handle_auth_urls(app: AppHandle, urls: Vec<url::Url>, last: Arc<Mutex<Option<String>>>) {
    for url in urls {
        if url.scheme() != "citadel" {
            continue;
        }
        let raw = url.to_string();
        if let Ok(mut seen) = last.lock() {
            if seen.as_deref() == Some(raw.as_str()) {
                continue;
            }
            *seen = Some(raw.clone());
        }
        let app_for_exchange = app.clone();
        tauri::async_runtime::spawn(async move {
            let auth = app_for_exchange.state::<AuthState>();
            let result = auth.exchange_callback(&raw).await;
            let event = match result {
                Ok(view) => AuthEvent {
                    view: Some(view),
                    error: None,
                },
                Err(error) => AuthEvent {
                    view: None,
                    error: Some(error),
                },
            };
            let _ = app_for_exchange.emit("citadel:auth", event);
        });
    }
}

#[tauri::command]
async fn begin_login(app: AppHandle, auth: State<'_, AuthState>) -> Result<(), String> {
    let url = auth.begin_login().await?;
    if app.opener().open_url(url, None::<&str>).is_err() {
        let _ = auth.cancel_login().await;
        return Err("Could not open the browser for sign in".to_string());
    }
    Ok(())
}

#[tauri::command]
async fn cancel_login(auth: State<'_, AuthState>) -> Result<(), String> {
    auth.cancel_login().await
}

#[tauri::command]
async fn verify_mfa(
    auth: State<'_, AuthState>,
    factor_id: String,
    code: String,
) -> Result<AuthView, String> {
    auth.verify_mfa(&factor_id, &code).await
}

#[tauri::command]
async fn session_status(auth: State<'_, AuthState>) -> Result<AuthView, String> {
    auth.view().await
}

#[tauri::command]
async fn sign_out(auth: State<'_, AuthState>) -> Result<(), String> {
    auth.sign_out().await
}

pub fn run() {
    let mut builder = tauri::Builder::default();
    #[cfg(desktop)]
    {
        // Must precede deep-link so a callback opened on an already running
        // instance reaches its existing window on Windows and Linux too.
        builder = builder.plugin(tauri_plugin_single_instance::init(|_app, _args, _cwd| {}));
    }
    builder
        .plugin(tauri_plugin_deep_link::init())
        .plugin(tauri_plugin_opener::init())
        .plugin(tauri_plugin_shell::init())
        .manage(AuthState::new())
        .manage(node::InitState::default())
        .setup(|app| {
            let handle = app.handle().clone();
            let last = Arc::new(Mutex::new(None));
            app.deep_link().on_open_url(move |event| {
                handle_auth_urls(handle.clone(), event.urls(), last.clone());
            });
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            begin_login,
            cancel_login,
            verify_mfa,
            session_status,
            sign_out,
            node::list_nodes,
            node::approve_device,
            node::local_status,
            node::service_status,
            node::reconcile_desktop_helper,
            node::service_action,
            node::diagnostics,
            node::init_this_computer,
            node::cancel_init,
        ])
        .build(tauri::generate_context!())
        .expect("Citadel desktop failed to start")
        .run(|app, event| {
            if matches!(
                event,
                tauri::RunEvent::ExitRequested { .. } | tauri::RunEvent::Exit
            ) {
                app.state::<node::InitState>().cancel();
            }
        });
}
