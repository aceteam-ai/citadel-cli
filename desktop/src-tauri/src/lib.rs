mod auth;
mod node;

use auth::{AuthState, AuthView};
use tauri::{AppHandle, State};
use tauri_plugin_opener::OpenerExt;

#[tauri::command]
fn begin_login(app: AppHandle) -> Result<(), String> {
    app.opener()
        .open_url(auth::login_url(), None::<&str>)
        .map_err(|_| "Could not open the browser for sign in".to_string())
}

#[tauri::command]
async fn complete_login(
    auth: State<'_, AuthState>,
    callback_url: String,
) -> Result<AuthView, String> {
    let code = auth::code_from_callback(&callback_url)?;
    auth.exchange_code(&code).await
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
        .invoke_handler(tauri::generate_handler![
            begin_login,
            complete_login,
            session_status,
            sign_out,
            node::list_nodes,
            node::approve_device,
            node::local_status,
            node::service_status,
            node::service_action,
            node::diagnostics,
            node::init_this_computer,
        ])
        .run(tauri::generate_context!())
        .expect("Citadel desktop failed to start");
}
