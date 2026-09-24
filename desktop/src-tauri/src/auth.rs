use keyring::{Entry, Error as KeyringError};
use reqwest::{Client, Method, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::Mutex;
use url::Url;

const API_BASE: &str = "https://aceteam.ai";
const CALLBACK: &str = "citadel://auth/callback";
const KEYRING_SERVICE: &str = "ai.aceteam.citadel";
const KEYRING_ACCOUNT: &str = "desktop-session";

pub struct AuthState {
    refresh_lock: Mutex<()>,
    client: Client,
}

impl AuthState {
    pub fn new() -> Self {
        Self {
            refresh_lock: Mutex::new(()),
            client: Client::builder()
                .timeout(Duration::from_secs(15))
                .build()
                .expect("HTTP client configuration"),
        }
    }

    pub async fn exchange_code(&self, code: &str) -> Result<AuthView, String> {
        let response = self
            .client
            .post(format!("{API_BASE}/api/auth/mobile/session-exchange"))
            .json(&json!({ "code": code }))
            .send()
            .await
            .map_err(|_| "Could not reach AceTeam to finish sign in".to_string())?;
        if !response.status().is_success() {
            return Err("The sign-in link expired. Please try again".to_string());
        }
        let payload: TokenResponse = response
            .json()
            .await
            .map_err(|_| "AceTeam returned an invalid session".to_string())?;
        let session = Session::from_response(payload, None);
        save_session(&session)?;
        Ok(AuthView::from(&session))
    }

    pub async fn view(&self) -> Result<AuthView, String> {
        match load_session()? {
            Some(session) => {
                if session.expires_at <= now_seconds() + 60 {
                    match self.access_token().await {
                        Ok(_) => Ok(AuthView::from(&load_session()?.ok_or("Session not found")?)),
                        Err(_) if load_session()?.is_none() => Ok(AuthView::signed_out()),
                        Err(error) => Err(error),
                    }
                } else {
                    Ok(AuthView::from(&session))
                }
            }
            None => Ok(AuthView::signed_out()),
        }
    }

    async fn access_token(&self) -> Result<String, String> {
        // Rotation is serialized: two UI requests cannot spend the same refresh
        // token and accidentally sign the user out.
        let _guard = self.refresh_lock.lock().await;
        let session = load_session()?.ok_or("Please sign in")?;
        if session.expires_at > now_seconds() + 60 {
            return Ok(session.access_token);
        }
        self.refresh_locked(session).await
    }

    async fn refresh_locked(&self, session: Session) -> Result<String, String> {
        let mut body = json!({ "refresh_token": session.refresh_token.clone() });
        if let Some(device_id) = &session.device_id {
            body["device_id"] = json!(device_id);
        }
        let response = self
            .client
            .post(format!("{API_BASE}/api/auth/mobile/refresh"))
            .json(&body)
            .send()
            .await
            .map_err(|_| "Could not refresh the AceTeam session".to_string())?;
        if response.status() == StatusCode::UNAUTHORIZED {
            clear_session()?;
            return Err("Your session expired. Please sign in again".to_string());
        }
        if !response.status().is_success() {
            return Err("Could not refresh the AceTeam session".to_string());
        }
        let payload: TokenResponse = response
            .json()
            .await
            .map_err(|_| "AceTeam returned an invalid session".to_string())?;
        let next = Session::from_response(payload, Some(session));
        let token = next.access_token.clone();
        save_session(&next)?;
        Ok(token)
    }

    pub async fn request(
        &self,
        method: Method,
        path: &str,
        body: Option<Value>,
    ) -> Result<Value, String> {
        let token = self.access_token().await?;
        let url = format!("{API_BASE}{path}");
        let mut request = self.client.request(method.clone(), &url).bearer_auth(token);
        if let Some(value) = &body {
            request = request.json(value);
        }
        let mut response = request
            .send()
            .await
            .map_err(|_| "AceTeam is unreachable".to_string())?;
        if response.status() == StatusCode::UNAUTHORIZED {
            // A server-revoked access token may still be inside its JWT lifetime.
            // Force one refresh and retry, matching the native TokenStore flow.
            let _guard = self.refresh_lock.lock().await;
            let mut session = load_session()?.ok_or("Please sign in")?;
            session.expires_at = 0;
            let token = self.refresh_locked(session).await?;
            let mut retry = self.client.request(method, &url).bearer_auth(token);
            if let Some(value) = body {
                retry = retry.json(&value);
            }
            response = retry
                .send()
                .await
                .map_err(|_| "AceTeam is unreachable".to_string())?;
        }
        if !response.status().is_success() {
            return Err(format!(
                "AceTeam request failed ({})",
                response.status().as_u16()
            ));
        }
        response
            .json()
            .await
            .map_err(|_| "AceTeam returned invalid data".to_string())
    }

    pub async fn sign_out(&self) -> Result<(), String> {
        if let Ok(token) = self.access_token().await {
            let _ = self
                .client
                .post(format!("{API_BASE}/api/auth/mobile/logout"))
                .bearer_auth(token)
                .json(&json!({}))
                .send()
                .await;
        }
        clear_session()
    }
}

#[derive(Clone, Serialize, Deserialize)]
struct Session {
    access_token: String,
    refresh_token: String,
    expires_at: u64,
    device_id: Option<String>,
    email: Option<String>,
}

#[derive(Deserialize)]
struct TokenResponse {
    access_token: String,
    refresh_token: String,
    expires_in: u64,
    device_id: Option<String>,
    user: Option<UserResponse>,
}

#[derive(Deserialize)]
struct UserResponse {
    email: Option<String>,
}

impl Session {
    fn from_response(response: TokenResponse, previous: Option<Session>) -> Self {
        Self {
            access_token: response.access_token,
            refresh_token: response.refresh_token,
            expires_at: now_seconds().saturating_add(response.expires_in),
            device_id: response
                .device_id
                .or_else(|| previous.as_ref().and_then(|p| p.device_id.clone())),
            email: response
                .user
                .and_then(|user| user.email)
                .or_else(|| previous.and_then(|p| p.email)),
        }
    }
}

#[derive(Serialize)]
pub struct AuthView {
    pub signed_in: bool,
    pub email: Option<String>,
}

impl AuthView {
    fn signed_out() -> Self {
        Self {
            signed_in: false,
            email: None,
        }
    }
}

impl From<&Session> for AuthView {
    fn from(session: &Session) -> Self {
        Self {
            signed_in: true,
            email: session.email.clone(),
        }
    }
}

fn now_seconds() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

fn entry() -> Result<Entry, String> {
    Entry::new(KEYRING_SERVICE, KEYRING_ACCOUNT)
        .map_err(|_| "System credential store is unavailable".to_string())
}

fn load_session() -> Result<Option<Session>, String> {
    match entry()?.get_password() {
        Ok(value) => serde_json::from_str(&value)
            .map(Some)
            .map_err(|_| "Saved session is invalid".to_string()),
        Err(KeyringError::NoEntry) => Ok(None),
        Err(_) => Err("Could not read the system credential store".to_string()),
    }
}

fn save_session(session: &Session) -> Result<(), String> {
    let value = serde_json::to_string(session).map_err(|_| "Could not save session".to_string())?;
    entry()?
        .set_password(&value)
        .map_err(|_| "Could not save to the system credential store".to_string())
}

fn clear_session() -> Result<(), String> {
    match entry()?.delete_credential() {
        Ok(()) | Err(KeyringError::NoEntry) => Ok(()),
        Err(_) => Err("Could not clear the system credential store".to_string()),
    }
}

pub fn code_from_callback(raw: &str) -> Result<String, String> {
    let url = Url::parse(raw).map_err(|_| "Invalid sign-in callback".to_string())?;
    if url.scheme() != "citadel"
        || url.host_str() != Some("auth")
        || url.path() != "/callback"
        || url.fragment().is_some()
    {
        return Err("Unexpected sign-in callback".to_string());
    }
    let pairs: Vec<_> = url.query_pairs().collect();
    if pairs.len() != 1 || pairs[0].0 != "code" || pairs[0].1.is_empty() {
        return Err("Sign-in callback is missing its code".to_string());
    }
    Ok(pairs[0].1.to_string())
}

pub fn login_url() -> String {
    let mut url = Url::parse(&format!("{API_BASE}/auth/app-login")).expect("fixed login URL");
    url.query_pairs_mut().append_pair("redirect_uri", CALLBACK);
    url.to_string()
}

pub fn valid_device_code(code: &str) -> bool {
    let bytes = code.as_bytes();
    bytes.len() == 9
        && bytes[4] == b'-'
        && bytes
            .iter()
            .enumerate()
            .all(|(index, byte)| index == 4 || byte.is_ascii_uppercase() || byte.is_ascii_digit())
}

#[cfg(test)]
mod tests {
    use super::{code_from_callback, valid_device_code};

    #[test]
    fn callback_accepts_only_the_exact_deep_link_and_single_code() {
        assert_eq!(
            code_from_callback("citadel://auth/callback?code=abc").unwrap(),
            "abc"
        );
        for bad in [
            "https://auth/callback?code=abc",
            "citadel://auth.evil/callback?code=abc",
            "citadel://auth/other?code=abc",
            "citadel://auth/callback?code=abc&code=def",
            "citadel://auth/callback?code=abc&next=https://evil.example",
            "citadel://auth/callback?code=abc#fragment",
        ] {
            assert!(code_from_callback(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn device_code_matches_the_node_contract() {
        assert!(valid_device_code("ABCD-1234"));
        assert!(!valid_device_code("abcd-1234"));
        assert!(!valid_device_code("ABCD-1234-extra"));
    }
}
