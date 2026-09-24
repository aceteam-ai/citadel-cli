use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine as _};
use keyring::{Entry, Error as KeyringError};
use reqwest::{Client, Method, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tokio::sync::Mutex;
use url::Url;

const API_BASE: &str = "https://aceteam.ai";
const CALLBACK: &str = "citadel://auth/callback";
const KEYRING_SERVICE: &str = "ai.aceteam.citadel";
const KEYRING_ACCOUNT: &str = "desktop-session";
const MFA_LIFETIME: Duration = Duration::from_secs(120);

struct LoginAttempt {
    state: String,
    verifier: String,
    expires_at: Instant,
}

struct PendingMfa {
    id: String,
    factors: Vec<MfaFactor>,
    expires_at: Instant,
}

#[derive(Default)]
struct AuthFlow {
    attempt: Option<LoginAttempt>,
    mfa: Option<PendingMfa>,
    completed_login: bool,
}

pub struct AuthState {
    // All exchange, refresh, MFA, and logout operations share one lock, so a
    // response can never overwrite a session cleared by a later sign-out.
    flow: Mutex<AuthFlow>,
    generation: AtomicU64,
    client: Client,
}

impl AuthState {
    pub fn new() -> Self {
        Self {
            flow: Mutex::new(AuthFlow::default()),
            generation: AtomicU64::new(0),
            client: Client::builder()
                .timeout(Duration::from_secs(15))
                .build()
                .expect("HTTP client configuration"),
        }
    }

    pub async fn begin_login(&self) -> Result<String, String> {
        let mut flow = self.flow.lock().await;
        self.cancel_locked(&mut flow).await?;
        flow.completed_login = false;
        let verifier = new_verifier()?;
        let challenge = URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()));
        let response = self
            .client
            .post(format!("{API_BASE}/api/auth/mobile/desktop/start"))
            .json(&json!({ "code_challenge": challenge }))
            .send()
            .await
            .map_err(|_| "Could not start AceTeam sign in".to_string())?;
        if !response.status().is_success() {
            return Err("Could not start AceTeam sign in".to_string());
        }
        let started: StartResponse = response
            .json()
            .await
            .map_err(|_| "AceTeam returned an invalid sign-in link".to_string())?;
        if !valid_hex(&started.state) || started.expires_in == 0 {
            return Err("AceTeam returned an invalid sign-in link".to_string());
        }
        validate_login_url(&started.login_url, &started.state)?;
        flow.attempt = Some(LoginAttempt {
            state: started.state,
            verifier,
            expires_at: Instant::now() + Duration::from_secs(started.expires_in.min(300)),
        });
        Ok(started.login_url)
    }

    pub async fn exchange_callback(&self, callback: &str) -> Result<AuthView, String> {
        let parsed = callback_values(callback);
        let mut flow = self.flow.lock().await;
        let (code, state) = match parsed {
            Ok(values) => values,
            Err(error) => {
                let _ = self.cancel_locked(&mut flow).await;
                return Err(error);
            }
        };
        let attempt = match take_attempt(&mut flow, &state) {
            Ok(attempt) => attempt,
            Err(Some(attempt)) => {
                let _ = self.cancel_handle("state", &attempt.state).await;
                return Err("This sign-in link is no longer active".to_string());
            }
            Err(None) => return Err("This sign-in link is no longer active".to_string()),
        };
        let generation = self.generation.load(Ordering::SeqCst);
        let response = self
            .client
            .post(format!("{API_BASE}/api/auth/mobile/session-exchange"))
            .json(&json!({ "code": code, "state": state, "code_verifier": attempt.verifier }))
            .send()
            .await;
        let response = match response {
            Ok(response) => response,
            Err(_) => {
                let _ = self.cancel_handle("state", &state).await;
                return Err("Could not reach AceTeam to finish sign in".to_string());
            }
        };
        if response.status() == StatusCode::ACCEPTED {
            let pending: MfaPendingResponse = response
                .json()
                .await
                .map_err(|_| "AceTeam returned an invalid verification request".to_string())?;
            if !pending.mfa_required
                || !valid_hex(&pending.pending_id)
                || pending.factors.is_empty()
            {
                return Err("AceTeam returned an invalid verification request".to_string());
            }
            if generation != self.generation.load(Ordering::SeqCst) {
                self.cancel_handle("pending_id", &pending.pending_id)
                    .await?;
                return Err("Sign in was cancelled".to_string());
            }
            let view = AuthView::pending(pending.factors.clone());
            flow.mfa = Some(PendingMfa {
                id: pending.pending_id,
                factors: pending.factors,
                expires_at: Instant::now() + MFA_LIFETIME,
            });
            return Ok(view);
        }
        if !response.status().is_success() {
            let _ = self.cancel_handle("state", &state).await;
            return Err("The sign-in link expired. Please try again".to_string());
        }
        let payload: TokenResponse = response
            .json()
            .await
            .map_err(|_| "AceTeam returned an invalid session".to_string())?;
        validate_tokens(&payload)?;
        if generation != self.generation.load(Ordering::SeqCst) {
            self.revoke_session(&payload.access_token).await?;
            return Err("Sign in was cancelled".to_string());
        }
        let session = Session::from_response(payload, None);
        save_session(&session)?;
        flow.completed_login = true;
        Ok(AuthView::from(&session))
    }

    pub async fn verify_mfa(&self, factor_id: &str, code: &str) -> Result<AuthView, String> {
        let mut flow = self.flow.lock().await;
        let pending = flow.mfa.as_ref().ok_or("No verification is pending")?;
        if Instant::now() >= pending.expires_at {
            flow.mfa = None;
            return Err("Verification expired. Please sign in again".to_string());
        }
        if !pending.factors.iter().any(|factor| factor.id == factor_id)
            || !(6..=8).contains(&code.len())
            || !code.bytes().all(|byte| byte.is_ascii_digit())
        {
            return Err("Choose a factor and enter its verification code".to_string());
        }
        let pending = flow.mfa.take().expect("checked pending MFA");
        let generation = self.generation.load(Ordering::SeqCst);
        let response = self
            .client
            .post(format!("{API_BASE}/api/auth/mobile/desktop/mfa"))
            .json(&json!({ "pending_id": pending.id, "factor_id": factor_id, "code": code }))
            .send()
            .await;
        let response = match response {
            Ok(response) => response,
            Err(_) => {
                let _ = self.cancel_handle("pending_id", &pending.id).await;
                return Err("Could not verify this sign in. Please start again".to_string());
            }
        };
        if !response.status().is_success() {
            return Err("Verification failed. Please sign in again".to_string());
        }
        let payload: TokenResponse = response
            .json()
            .await
            .map_err(|_| "AceTeam returned an invalid session".to_string())?;
        validate_tokens(&payload)?;
        if generation != self.generation.load(Ordering::SeqCst) {
            self.revoke_session(&payload.access_token).await?;
            return Err("Sign in was cancelled".to_string());
        }
        let session = Session::from_response(payload, None);
        save_session(&session)?;
        flow.completed_login = true;
        Ok(AuthView::from(&session))
    }

    pub async fn cancel_login(&self) -> Result<(), String> {
        self.generation.fetch_add(1, Ordering::SeqCst);
        let mut flow = self.flow.lock().await;
        let cancel_result = self.cancel_locked(&mut flow).await;
        let completed_login = std::mem::take(&mut flow.completed_login);
        drop(flow);
        // If the exchange completed immediately before cancellation, revoke
        // the just-created session as well as clearing its local copy.
        let session_result = if completed_login {
            self.sign_out().await
        } else {
            Ok(())
        };
        cancel_result?;
        session_result
    }

    async fn cancel_locked(&self, flow: &mut AuthFlow) -> Result<(), String> {
        let attempt = flow.attempt.take();
        let mfa = flow.mfa.take();
        if let Some(mfa) = mfa {
            self.cancel_handle("pending_id", &mfa.id).await?;
        }
        if let Some(attempt) = attempt {
            self.cancel_handle("state", &attempt.state).await?;
        }
        Ok(())
    }

    async fn cancel_handle(&self, key: &str, value: &str) -> Result<(), String> {
        let mut body = serde_json::Map::new();
        body.insert(key.to_string(), json!(value));
        let response = self
            .client
            .post(format!("{API_BASE}/api/auth/mobile/desktop/cancel"))
            .json(&body)
            .send()
            .await
            .map_err(|_| "Could not cancel the remote sign in".to_string())?;
        if response.status().is_success() {
            Ok(())
        } else {
            Err("Could not cancel the remote sign in".to_string())
        }
    }

    pub async fn view(&self) -> Result<AuthView, String> {
        let mut flow = self.flow.lock().await;
        if let Some(pending) = &flow.mfa {
            if Instant::now() < pending.expires_at {
                return Ok(AuthView::pending(pending.factors.clone()));
            }
        }
        flow.mfa = None;
        match load_session()? {
            Some(session) if session.expires_at <= now_seconds() + 60 => {
                match self.access_token_locked().await {
                    Ok(_) => Ok(AuthView::from(&load_session()?.ok_or("Session not found")?)),
                    Err(_) if load_session()?.is_none() => Ok(AuthView::signed_out()),
                    Err(error) => Err(error),
                }
            }
            Some(session) => Ok(AuthView::from(&session)),
            None => Ok(AuthView::signed_out()),
        }
    }

    async fn access_token(&self) -> Result<String, String> {
        let _guard = self.flow.lock().await;
        self.access_token_locked().await
    }

    async fn access_token_locked(&self) -> Result<String, String> {
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
        validate_tokens(&payload)?;
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
            let _guard = self.flow.lock().await;
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
        self.generation.fetch_add(1, Ordering::SeqCst);
        let mut flow = self.flow.lock().await;
        let cancel_result = self.cancel_locked(&mut flow).await;
        flow.completed_login = false;
        let session = load_session();
        let remote_result = if matches!(session.as_ref(), Ok(Some(_))) {
            match self.access_token_locked().await {
                Ok(token) => self.revoke_session(&token).await,
                Err(error) => Err(error),
            }
        } else {
            session.map(|_| ())
        };
        let local_result = clear_session();
        local_result?;
        cancel_result?;
        remote_result
    }

    async fn revoke_session(&self, token: &str) -> Result<(), String> {
        for attempt in 0..2 {
            let result = self
                .client
                .post(format!("{API_BASE}/api/auth/mobile/logout"))
                .bearer_auth(token)
                .json(&json!({}))
                .send()
                .await;
            match result {
                Ok(response) if response.status().is_success() => return Ok(()),
                Ok(response) if attempt == 0 && response.status().is_server_error() => continue,
                Err(_) if attempt == 0 => continue,
                _ => break,
            }
        }
        Err("AceTeam could not revoke this session. Review active sessions in AceTeam".to_string())
    }
}

#[derive(Deserialize)]
struct StartResponse {
    state: String,
    login_url: String,
    expires_in: u64,
}

#[derive(Clone, Serialize, Deserialize)]
pub struct MfaFactor {
    pub id: String,
    pub friendly_name: Option<String>,
}

#[derive(Deserialize)]
struct MfaPendingResponse {
    mfa_required: bool,
    pending_id: String,
    factors: Vec<MfaFactor>,
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

fn validate_tokens(response: &TokenResponse) -> Result<(), String> {
    if response.access_token.is_empty()
        || response.refresh_token.is_empty()
        || response.expires_in == 0
    {
        Err("AceTeam returned an invalid session".to_string())
    } else {
        Ok(())
    }
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

#[derive(Clone, Serialize)]
pub struct AuthView {
    pub signed_in: bool,
    pub email: Option<String>,
    pub mfa_pending: bool,
    pub factors: Vec<MfaFactor>,
}

impl AuthView {
    fn signed_out() -> Self {
        Self {
            signed_in: false,
            email: None,
            mfa_pending: false,
            factors: vec![],
        }
    }

    fn pending(factors: Vec<MfaFactor>) -> Self {
        Self {
            signed_in: false,
            email: None,
            mfa_pending: true,
            factors,
        }
    }
}

impl From<&Session> for AuthView {
    fn from(session: &Session) -> Self {
        Self {
            signed_in: true,
            email: session.email.clone(),
            mfa_pending: false,
            factors: vec![],
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

fn valid_hex(value: &str) -> bool {
    value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn take_attempt(flow: &mut AuthFlow, state: &str) -> Result<LoginAttempt, Option<LoginAttempt>> {
    match flow.attempt.take() {
        Some(attempt) if attempt.state == state && Instant::now() < attempt.expires_at => {
            Ok(attempt)
        }
        other => Err(other),
    }
}

fn new_verifier() -> Result<String, String> {
    let mut bytes = [0u8; 32];
    getrandom::fill(&mut bytes).map_err(|_| "Could not start secure sign in".to_string())?;
    Ok(URL_SAFE_NO_PAD.encode(bytes))
}

fn validate_login_url(raw: &str, state: &str) -> Result<(), String> {
    let url =
        Url::parse(raw).map_err(|_| "AceTeam returned an invalid sign-in link".to_string())?;
    if url.scheme() != "https"
        || url.host_str() != Some("aceteam.ai")
        || url.port().is_some()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.path() != "/auth/app-login"
        || url.fragment().is_some()
    {
        return Err("AceTeam returned an invalid sign-in link".to_string());
    }
    let pairs: Vec<_> = url.query_pairs().collect();
    if pairs.len() != 2
        || pairs
            .iter()
            .filter(|(key, value)| key == "state" && value == state)
            .count()
            != 1
        || pairs
            .iter()
            .filter(|(key, value)| key == "redirect_uri" && value == CALLBACK)
            .count()
            != 1
    {
        return Err("AceTeam returned an invalid sign-in link".to_string());
    }
    Ok(())
}

pub fn callback_values(raw: &str) -> Result<(String, String), String> {
    let url = Url::parse(raw).map_err(|_| "Invalid sign-in callback".to_string())?;
    if url.scheme() != "citadel"
        || url.host_str() != Some("auth")
        || url.port().is_some()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.path() != "/callback"
        || url.fragment().is_some()
    {
        return Err("Unexpected sign-in callback".to_string());
    }
    let pairs: Vec<_> = url.query_pairs().collect();
    if pairs.len() != 2 {
        return Err("Unexpected sign-in callback".to_string());
    }
    let code = pairs
        .iter()
        .find(|(key, _)| key == "code")
        .map(|(_, value)| value.to_string());
    let state = pairs
        .iter()
        .find(|(key, _)| key == "state")
        .map(|(_, value)| value.to_string());
    match (code, state) {
        (Some(code), Some(state)) if valid_hex(&code) && valid_hex(&state) => Ok((code, state)),
        _ => Err("Unexpected sign-in callback".to_string()),
    }
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
    use super::{
        callback_values, new_verifier, take_attempt, valid_device_code, validate_login_url,
        AuthFlow, LoginAttempt, CALLBACK,
    };
    use std::time::{Duration, Instant};

    #[test]
    fn attempt_requires_exact_state_and_is_consumed_once() {
        let mut flow = AuthFlow {
            attempt: Some(LoginAttempt {
                state: "a".repeat(64),
                verifier: "v".repeat(43),
                expires_at: Instant::now() + Duration::from_secs(60),
            }),
            mfa: None,
            completed_login: false,
        };
        assert!(take_attempt(&mut flow, &"b".repeat(64)).is_err());
        assert!(flow.attempt.is_none());
        flow.attempt = Some(LoginAttempt {
            state: "a".repeat(64),
            verifier: "v".repeat(43),
            expires_at: Instant::now() + Duration::from_secs(60),
        });
        assert!(take_attempt(&mut flow, &"a".repeat(64)).is_ok());
        assert!(take_attempt(&mut flow, &"a".repeat(64)).is_err());
    }

    #[test]
    fn callback_requires_exact_authority_code_and_state() {
        let code = "a".repeat(64);
        let state = "b".repeat(64);
        let good = format!("citadel://auth/callback?code={code}&state={state}");
        assert_eq!(
            callback_values(&good).unwrap(),
            (code.clone(), state.clone())
        );
        for bad in [
            format!("https://auth/callback?code={code}&state={state}"),
            format!("citadel://auth.evil/callback?code={code}&state={state}"),
            format!("citadel://evil@auth/callback?code={code}&state={state}"),
            format!("citadel://auth/other?code={code}&state={state}"),
            format!("citadel://auth/callback?code={code}&code={code}&state={state}"),
            format!("citadel://auth/callback?code=short&state={state}"),
            format!("citadel://auth/callback?code={code}&state={state}#fragment"),
        ] {
            assert!(callback_values(&bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn verifier_and_hosted_url_follow_contract() {
        let verifier = new_verifier().unwrap();
        assert_eq!(verifier.len(), 43);
        assert!(verifier
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_'));
        let state = "a".repeat(64);
        let url =
            format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}");
        assert!(validate_login_url(&url, &state).is_ok());
        assert!(validate_login_url("https://evil.example/auth/app-login", &state).is_err());
    }

    #[test]
    fn device_code_matches_the_node_contract() {
        assert!(valid_device_code("ABCD-1234"));
        assert!(!valid_device_code("abcd-1234"));
        assert!(!valid_device_code("ABCD-1234-extra"));
    }
}
