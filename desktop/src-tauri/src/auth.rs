use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine as _};
use keyring::{Entry, Error as KeyringError};
use reqwest::{Client, Method, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::future::Future;
use std::pin::Pin;
use std::sync::{Mutex as AuthorityMutex, MutexGuard};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use tokio::sync::Mutex;
use url::Url;

const API_BASE: &str = "https://aceteam.ai";
const CALLBACK: &str = "citadel://auth/callback";
const KEYRING_SERVICE: &str = "ai.aceteam.citadel";
const KEYRING_ACCOUNT: &str = "desktop-session";
const MFA_LIFETIME: Duration = Duration::from_secs(120);
const CHANGED: &str = "Authentication changed. Please try again";
const UNAVAILABLE: &str = "Sign-in is unavailable. Review active sessions in AceTeam";
const CLEANUP_UNCONFIRMED: &str = "Local sign-in is unavailable. Remote sign-in cleanup could not be confirmed. Review active sessions in AceTeam";
const TERMINAL: u64 = u64::MAX;

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
    initialized: bool,
    attempt: Option<LoginAttempt>,
    mfa: Option<PendingMfa>,
    login_receipt: Option<LoginReceipt>,
    session: Option<OwnedSession>,
    uncertain_session: Option<Session>,
    candidate_origin: Option<CandidateOrigin>,
    // Exactly two known credential slots: session and uncertain_session. Local
    // deletion is not remote revocation and cannot discard either obligation.
    store_cleared: bool,
    session_revoked: bool,
    candidate_revoked: bool,
    cleanup: Option<LoginCleanup>,
    // Process-local only. A handle ACK cannot resolve unknown issuance.
    unknown_issuance: bool,
    unknown_refresh_origin: Option<AuthOrigin>,
}
struct CleanupHandle { value: String, acknowledged: bool }
enum CandidateOrigin { Lineage(u64), Login(LoginReceipt) }
struct LoginCleanup {
    receipt: LoginReceipt,
    state: Option<CleanupHandle>,
    pending: Option<CleanupHandle>,
    browser_exposed: bool,
    exchange_admitted: bool,
    mfa_admitted: bool,
    hidden_issued: bool,
    credential_received: bool,
}
impl LoginCleanup {
    fn new(receipt: LoginReceipt) -> Self {
        Self { receipt, state: None, pending: None, browser_exposed: false,
            exchange_admitted: false, mfa_admitted: false, hidden_issued: false,
            credential_received: false }
    }
    fn unresolved_issuance(&self) -> bool {
        self.hidden_issued || ((self.browser_exposed || self.exchange_admitted
            || self.mfa_admitted) && !self.credential_received)
    }
}
struct OwnedSession {
    lineage: u64,
    revision: u64,
    value: Session,
}
#[derive(Clone, PartialEq, Eq)]
struct LoginReceipt {
    caller_epoch: u64,
    pending_epoch: u64,
    operation_id: String,
    installed_lineage: Option<u64>,
}
#[derive(Clone, Copy, PartialEq, Eq)]
enum Phase {
    Uninitialized,
    SignedOut,
    Starting,
    Attempt,
    Exchanging,
    Mfa,
    Session,
    Closing,
    Disabled,
}
#[derive(Clone, Copy, PartialEq, Eq)]
enum ControlKind { Begin, Cancel, Logout }
#[derive(Clone, PartialEq, Eq)]
struct Owner {
    kind: ControlKind,
    operation_id: String,
    caller_epoch: u64,
    reserved_epoch: u64,
}
#[derive(Clone)]
struct Reservation {
    owner: Owner,
    initial_view: AuthView,
    departing_receipt: Option<LoginReceipt>,
    departing_lineage: Option<u64>,
    // Only a sign_out admission that validated its exact target before an
    // overflowing increment may use ordinary E with reserved_auth_epoch=null.
    overflow_cleanup: bool,
}
struct Authority {
    epoch: u64,
    revision: u64,
    phase: Phase,
    owner: Option<Owner>,
    terminal_owner: Option<Owner>,
    receipt: Option<LoginReceipt>,
    lineage: Option<u64>,
    // Exact known credential retained after a proven 401 local removal. It is
    // cleanup custody only, never an authenticated admission lineage.
    cleanup_lineage: Option<u64>,
    view: AuthView,
}
impl Default for Authority {
    fn default() -> Self {
        Self {
            epoch: 0, revision: 0, phase: Phase::Uninitialized,
            owner: None, terminal_owner: None, receipt: None, lineage: None, cleanup_lineage: None,
            view: AuthView::empty(0, 0),
        }
    }
}
impl Authority {
    fn disable(&mut self) {
        if self.phase != Phase::Disabled { self.terminal_owner = self.owner.clone(); }
        self.epoch = TERMINAL;
        self.revision = TERMINAL;
        self.phase = Phase::Disabled;
        self.owner = None;
        self.lineage = None;
        self.view = AuthView::empty(TERMINAL, TERMINAL);
    }
    fn advance(&mut self, epoch: bool) -> Result<(), String> {
        let next_e = if epoch { self.epoch.checked_add(1) } else { Some(self.epoch) };
        let next_v = self.revision.checked_add(1);
        match (next_e, next_v) {
            (Some(e), Some(v)) if e > 0 && e < TERMINAL && v < TERMINAL => {
                self.epoch = e;
                self.revision = v;
                self.view.auth_epoch = e.to_string();
                self.view.view_revision = v.to_string();
                Ok(())
            }
            _ => { self.disable(); Err(UNAVAILABLE.into()) }
        }
    }
    fn signed_out(&mut self, phase: Phase) {
        self.phase = phase;
        self.lineage = None;
        self.view.signed_in = false;
        self.view.email = None;
        self.view.mfa_pending = false;
        self.view.factors.clear();
        self.view.cancel_target = self.receipt.as_ref().map(LoginCancelTarget::from);
    }
    fn owns(&self, owner: &Owner) -> bool {
        self.phase != Phase::Disabled && self.epoch == owner.reserved_epoch
            && self.owner.as_ref() == Some(owner)
    }
    fn accepts(&self, origin: &AuthOrigin) -> bool {
        self.phase == Phase::Session && self.epoch == origin.epoch
            && self.lineage == Some(origin.lineage)
    }
}
#[derive(Clone)]
pub(crate) struct AuthOrigin {
    epoch: u64,
    lineage: u64,
}
impl AuthOrigin {
    pub(crate) fn epoch_text(&self) -> String { self.epoch.to_string() }
}
#[derive(Clone, Serialize)]
pub struct LoginCancelTarget {
    pub expected_auth_epoch: String,
    pub operation_id: String,
}
impl From<&LoginReceipt> for LoginCancelTarget {
    fn from(receipt: &LoginReceipt) -> Self {
        Self {
            expected_auth_epoch: receipt.caller_epoch.to_string(),
            operation_id: receipt.operation_id.clone(),
        }
    }
}
#[derive(Clone, Serialize)]
pub struct AuthView {
    pub auth_epoch: String,
    pub view_revision: String,
    pub signed_in: bool,
    pub email: Option<String>,
    pub mfa_pending: bool,
    pub factors: Vec<MfaFactor>,
    pub cancel_target: Option<LoginCancelTarget>,
}
impl AuthView {
    fn empty(epoch: u64, revision: u64) -> Self {
        Self {
            auth_epoch: epoch.to_string(), view_revision: revision.to_string(),
            signed_in: false, email: None, mfa_pending: false,
            factors: vec![], cancel_target: None,
        }
    }
}
#[derive(Clone, Serialize)]
pub struct AuthOutcome {
    pub operation_id: String,
    pub originating_auth_epoch: String,
    pub reserved_auth_epoch: Option<String>,
    pub view: AuthView,
    pub error: Option<String>,
    pub superseded: bool,
}
#[derive(Serialize)]
pub(crate) struct BoundResult<T: Serialize> {
    pub auth_epoch: String,
    pub view_revision: String,
    pub value: T,
}
pub(crate) struct BegunLogin {
    outcome: AuthOutcome,
    owner: Reservation,
    url: Option<String>,
}
#[derive(Clone, Copy)]
enum Route { Start, Exchange, Mfa, Refresh, Cancel, Logout, Nodes, Approve }
impl Route {
    fn path(self) -> &'static str {
        match self {
            Self::Start => "/api/auth/mobile/desktop/start",
            Self::Exchange => "/api/auth/mobile/session-exchange",
            Self::Mfa => "/api/auth/mobile/desktop/mfa",
            Self::Refresh => "/api/auth/mobile/refresh",
            Self::Cancel => "/api/auth/mobile/desktop/cancel",
            Self::Logout => "/api/auth/mobile/logout",
            Self::Nodes => "/api/fabric/nodes",
            Self::Approve => "/api/fabric/device-auth/approve",
        }
    }
    fn method(self) -> Method {
        if matches!(self, Self::Nodes) { Method::GET } else { Method::POST }
    }
}
struct WireResponse { status: StatusCode, body: Value }
type PendingResponse = Pin<Box<dyn Future<Output = Result<WireResponse, String>> + Send>>;
enum RawResponse {
    Production(reqwest::Response),
    #[cfg(test)]
    Scripted { status: StatusCode, body: Vec<u8> },
    #[cfg(test)]
    DeferredBody { status: StatusCode,
        started: tokio::sync::oneshot::Sender<()>,
        body: tokio::sync::oneshot::Receiver<Result<Vec<u8>, String>> },
}
impl RawResponse {
    fn status(&self) -> StatusCode {
        match self {
            Self::Production(response) => response.status(),
            #[cfg(test)]
            Self::Scripted { status, .. } => *status,
            #[cfg(test)]
            Self::DeferredBody { status, .. } => *status,
        }
    }
    async fn json(self) -> Result<Value, String> {
        match self {
            Self::Production(response) => response.json::<Value>().await
                .map_err(|_| "AceTeam returned invalid data".to_string()),
            #[cfg(test)]
            Self::Scripted { body, .. } => serde_json::from_slice(&body)
                .map_err(|_| "AceTeam returned invalid data".to_string()),
            #[cfg(test)]
            Self::DeferredBody { started, body, .. } => {
                let _ = started.send(());
                let bytes = body.await.map_err(|_| "Fixture body was not delivered".to_string())??;
                serde_json::from_slice(&bytes).map_err(|_| "AceTeam returned invalid data".to_string())
            }
        }
    }
}
async fn read_response(route: Route, response: RawResponse) -> Result<WireResponse, String> {
    let status = response.status();
    // Cleanup and rejection decisions require status, not a JSON body. In
    // particular an empty/non-JSON 401 must still invalidate its owned session.
    let body = if !status.is_success() || matches!(route, Route::Cancel | Route::Logout) {
        Value::Null
    } else {
        response.json().await?
    };
    Ok(WireResponse { status, body })
}
enum HttpOwner<'a> { Control(&'a Owner), Session(&'a AuthOrigin) }

pub struct AuthState {
    // Only this short synchronous gate linearizes invalidation, store commits,
    // admission, and event handoff. It is never held across an await.
    flow: Mutex<AuthFlow>,
    authority: AuthorityMutex<Authority>,
    client: Option<Client>,
    #[cfg(test)]
    test_io: Option<std::sync::Arc<TestIo>>,
}
impl AuthState {
    pub fn new() -> Self {
        Self {
            flow: Mutex::new(AuthFlow::default()),
            authority: AuthorityMutex::new(Authority::default()),
            client: Some(Client::builder().timeout(Duration::from_secs(15))
                .build().expect("HTTP client configuration")),
            #[cfg(test)]
            test_io: None,
        }
    }
    fn gate(&self) -> MutexGuard<'_, Authority> {
        match self.authority.lock() {
            Ok(guard) => guard,
            Err(poisoned) => {
                let mut guard = poisoned.into_inner();
                guard.disable();
                guard
            }
        }
    }
    fn read_store(&self) -> Result<Option<Session>, String> {
        #[cfg(test)]
        if let Some(io) = &self.test_io { return io.read(); }
        load_session()
    }
    fn write_store(&self, session: &Session) -> Result<(), String> {
        #[cfg(test)]
        if let Some(io) = &self.test_io { return io.write(session); }
        save_session(session)
    }
    fn delete_store(&self) -> Result<(), String> {
        #[cfg(test)]
        if let Some(io) = &self.test_io { return io.clear(); }
        clear_session()
    }
    fn consistent(&self, flow: &AuthFlow, gate: &mut Authority) -> Result<(), String> {
        let expected = flow.session.as_ref().filter(|_| !flow.store_cleared).map(|s| &s.value);
        match self.read_store() {
            Ok(actual) if actual.as_ref() == expected => Ok(()),
            _ => { gate.disable(); Err(UNAVAILABLE.into()) }
        }
    }
    fn start_request(&self, route: Route, token: Option<String>, body: Value) -> PendingResponse {
        #[cfg(test)]
        if let Some(io) = &self.test_io { return io.start(route, token, body); }
        let Some(client) = &self.client else {
            return Box::pin(async { Err(UNAVAILABLE.into()) });
        };
        let mut request = client.request(route.method(), format!("{API_BASE}{}", route.path()));
        if let Some(token) = token { request = request.bearer_auth(token); }
        if !matches!(route, Route::Nodes) { request = request.json(&body); }
        // Construct/handoff once while the caller holds the authority gate.
        // Polling and network completion occur outside it and are not cancelled
        // by local invalidation.
        let pending = request.send();
        Box::pin(async move {
            let response = pending.await.map_err(|_| "AceTeam is unreachable".to_string())?;
            read_response(route, RawResponse::Production(response)).await
        })
    }
    fn admit_http(&self, owner: HttpOwner<'_>, route: Route, token: Option<String>, body: Value)
        -> Result<PendingResponse, String>
    {
        let gate = self.gate();
        let accepted = match owner {
            HttpOwner::Control(owner) => gate.owns(owner),
            HttpOwner::Session(origin) => gate.accepts(origin),
        };
        if !accepted { return Err(CHANGED.into()); }
        Ok(self.start_request(route, token, body))
    }
    // Cleanup is closed to these exact fixed endpoints/owned arguments. It
    // never consults CURRENT credentials or mints new application authority.
    async fn cancel_exact(&self, field: &'static str, handle: &str) -> Result<(), String> {
        let mut body = serde_json::Map::new();
        body.insert(field.into(), Value::String(handle.into()));
        let pending = { let _gate = self.gate(); self.start_request(Route::Cancel, None, Value::Object(body)) };
        let response = pending.await?;
        if response.status.is_success() { Ok(()) }
        else { Err("Could not cancel the remote sign in".into()) }
    }
    async fn revoke_exact(&self, token: &str) -> Result<(), String> {
        for attempt in 0..2 {
            let pending = { let _gate = self.gate(); self.start_request(Route::Logout, Some(token.into()), json!({})) };
            match pending.await {
                Ok(response) if response.status.is_success() => return Ok(()),
                Ok(response) if attempt == 0 && response.status.is_server_error() => (),
                Err(_) if attempt == 0 => (),
                _ => break,
            }
        }
        Err("AceTeam could not revoke this session. Review active sessions in AceTeam".into())
    }
    pub async fn view(&self) -> Result<AuthView, String> {
        let mut flow = self.flow.lock().await;
        let mut expired_login = false;
        {
            let mut gate = self.gate();
            if gate.phase == Phase::Disabled { return Ok(gate.view.clone()); }
            if !flow.initialized {
                flow.initialized = true;
                match self.read_store() {
                    Ok(Some(session)) if !session.access_token.is_empty() && !session.refresh_token.is_empty() => {
                        gate.epoch = 1; gate.revision = 1; gate.phase = Phase::Session;
                        gate.lineage = Some(1);
                        gate.view = AuthView::empty(1, 1);
                        gate.view.signed_in = true;
                        gate.view.email = session.email.clone();
                        flow.session = Some(OwnedSession { lineage: 1, revision: 0, value: session });
                    }
                    Ok(None) => {
                        gate.epoch = 1; gate.revision = 1;
                        gate.view = AuthView::empty(1, 1);
                        gate.phase = Phase::SignedOut;
                    }
                    _ => gate.disable(),
                }
            } else if self.consistent(&flow, &mut gate).is_err() {
                return Ok(gate.view.clone());
            }
            let expired_mfa = gate.phase == Phase::Mfa && flow.mfa.as_ref()
                .is_some_and(|pending| Instant::now() >= pending.expires_at);
            let expired_attempt = gate.phase == Phase::Attempt && flow.attempt.as_ref()
                .is_some_and(|attempt| Instant::now() >= attempt.expires_at);
            if expired_mfa || expired_attempt {
                if gate.advance(false).is_err() { return Ok(gate.view.clone()); }
                gate.owner = None;
                gate.receipt = None;
                gate.signed_out(Phase::SignedOut);
                expired_login = true;
            }
        }
        if expired_login && self.clean_owned(&mut flow).await.is_err() {
            self.gate().disable();
            return Ok(self.gate().view.clone());
        }
        let origin = {
            let gate = self.gate();
            flow.session.as_ref().filter(|_| gate.phase == Phase::Session)
                .filter(|s| s.value.expires_at <= now_seconds().saturating_add(60))
                .map(|s| AuthOrigin { epoch: gate.epoch, lineage: s.lineage })
        };
        if let Some(origin) = origin {
            if let Err(error) = self.refresh_locked(&mut flow, &origin).await {
                let gate = self.gate();
                if gate.accepts(&origin) { return Err(error); }
                // Disabled/store failures return their terminal view directly;
                // an exact 401 clear returns the new signed-out view.
            }
        }
        Ok(self.gate().view.clone())
    }
    pub(crate) async fn capture_origin(&self, expected: &str) -> Result<AuthOrigin, String> {
        let expected = parse_epoch(expected, false)?;
        let flow = self.flow.lock().await;
        let mut gate = self.gate();
        if !flow.initialized || gate.epoch != expected || gate.phase != Phase::Session {
            return Err(CHANGED.into());
        }
        self.consistent(&flow, &mut gate)?;
        let session = flow.session.as_ref().ok_or(CHANGED)?;
        if gate.lineage != Some(session.lineage) { return Err(CHANGED.into()); }
        Ok(AuthOrigin { epoch: expected, lineage: session.lineage })
    }
    pub(crate) fn finish_bound<T: Serialize>(&self, origin: &AuthOrigin, value: T)
        -> Result<BoundResult<T>, String>
    {
        let gate = self.gate();
        if !gate.accepts(origin) { return Err(CHANGED.into()); }
        Ok(BoundResult { auth_epoch: gate.epoch.to_string(),
            view_revision: gate.revision.to_string(), value })
    }
    async fn refresh_locked(&self, flow: &mut AuthFlow, origin: &AuthOrigin) -> Result<String, String> {
        let (previous, revision) = {
            let mut gate = self.gate();
            if !gate.accepts(origin) { return Err(CHANGED.into()); }
            self.consistent(flow, &mut gate)?;
            if flow.uncertain_session.is_some() || flow.candidate_origin.is_some() || flow.unknown_issuance {
                gate.disable();
                return Err(CLEANUP_UNCONFIRMED.into());
            }
            let owned = flow.session.as_ref().ok_or(CHANGED)?;
            if owned.revision.checked_add(1).map_or(true, |n| n >= TERMINAL)
                || gate.revision.checked_add(1).map_or(true, |n| n >= TERMINAL) {
                gate.disable();
                return Err(UNAVAILABLE.into());
            }
            (owned.value.clone(), owned.revision)
        };
        let mut body = json!({ "refresh_token": previous.refresh_token });
        if let Some(device_id) = &previous.device_id { body["device_id"] = json!(device_id); }
        #[cfg(test)]
        if let Some(io) = &self.test_io { io.pause(TestPoint::RefreshAdmission).await; }
        let pending = self.admit_http(HttpOwner::Session(origin), Route::Refresh, None, body)?;
        let response = match pending.await {
            Ok(response) => response,
            Err(_) => {
                flow.unknown_issuance = true;
                flow.unknown_refresh_origin = Some(origin.clone());
                self.gate().disable();
                return Err(CLEANUP_UNCONFIRMED.into());
            }
        };
        if response.status == StatusCode::UNAUTHORIZED {
            let mut gate = self.gate();
            if !gate.accepts(origin) { return Err(CHANGED.into()); }
            self.consistent(flow, &mut gate)?;
            gate.advance(true)?;
            gate.owner = None; gate.receipt = None;
            gate.cleanup_lineage = Some(origin.lineage);
            gate.signed_out(Phase::Closing);
            if self.delete_store().is_err() { gate.disable(); return Err(UNAVAILABLE.into()); }
            flow.store_cleared = true;
            flow.login_receipt = None;
            gate.advance(false)?;
            gate.signed_out(Phase::SignedOut);
            return Err("Your session expired. Please sign in again".into());
        }
        if !response.status.is_success() {
            flow.unknown_issuance = true;
            flow.unknown_refresh_origin = Some(origin.clone());
            self.gate().disable();
            return Err(CLEANUP_UNCONFIRMED.into());
        }
        let payload = match serde_json::from_value::<TokenResponse>(response.body) {
            Ok(payload) if validate_tokens(&payload).is_ok() => payload,
            _ => {
                flow.unknown_issuance = true;
                flow.unknown_refresh_origin = Some(origin.clone());
                self.gate().disable();
                return Err(CLEANUP_UNCONFIRMED.into());
            }
        };
        let next = Session::from_response(payload, Some(previous));
        flow.uncertain_session = Some(next.clone());
        flow.candidate_origin = Some(CandidateOrigin::Lineage(origin.lineage));
        flow.candidate_revoked = false;
        #[cfg(test)]
        if let Some(io) = &self.test_io { io.pause(TestPoint::RefreshCommit).await; }
        let committed = {
            let mut gate = self.gate();
            if !gate.accepts(origin) { Err(CHANGED.to_string()) }
            else if self.consistent(flow, &mut gate).is_err() { Err(UNAVAILABLE.to_string()) }
            else if let Some(next_revision) = revision.checked_add(1).filter(|n| *n < TERMINAL) {
                if gate.advance(false).is_err() { Err(UNAVAILABLE.to_string()) }
                else {
                    flow.store_cleared = false;
                    if self.write_store(&next).is_err() {
                        gate.disable();
                        Err(UNAVAILABLE.to_string())
                    } else {
                        flow.uncertain_session = None;
                        flow.candidate_origin = None;
                        flow.session = Some(OwnedSession { lineage: origin.lineage,
                            revision: next_revision, value: next.clone() });
                        flow.store_cleared = false;
                        flow.session_revoked = false;
                        gate.view.email = next.email.clone();
                        Ok(next.access_token.clone())
                    }
                }
            } else { gate.disable(); Err(UNAVAILABLE.to_string()) }
        };
        // The synchronous guard has a lexical scope ending before cleanup IO.
        if committed.is_err() {
            if self.revoke_exact(&next.access_token).await.is_err() {
                self.gate().disable();
            } else { flow.candidate_revoked = true; }
        }
        committed
    }
    pub(crate) async fn request_bound(&self, origin: &AuthOrigin, method: Method,
        path: &str, body: Option<Value>) -> Result<BoundResult<Value>, String>
    {
        let route = if method == Method::GET && path == "/api/fabric/nodes" {
            Route::Nodes
        } else if method == Method::POST && path == "/api/fabric/device-auth/approve" {
            Route::Approve
        } else { return Err("Unsupported AceTeam request".into()); };
        let (token, sent_revision) = {
            let mut flow = self.flow.lock().await;
            {
                let mut gate = self.gate();
                if !gate.accepts(origin) { return Err(CHANGED.into()); }
                self.consistent(&flow, &mut gate)?;
            }
            if flow.session.as_ref().ok_or(CHANGED)?.value.expires_at <= now_seconds().saturating_add(60) {
                self.refresh_locked(&mut flow, origin).await?;
            }
            let gate = self.gate();
            if !gate.accepts(origin) { return Err(CHANGED.into()); }
            let owned = flow.session.as_ref().ok_or(CHANGED)?;
            (owned.value.access_token.clone(), owned.revision)
        };
        let original_body = body.unwrap_or_else(|| json!({}));
        #[cfg(test)]
        if let Some(io) = &self.test_io { io.pause(TestPoint::OriginalAdmission).await; }
        let mut response = self.admit_http(HttpOwner::Session(origin), route,
            Some(token), original_body.clone())?.await?;
        if response.status == StatusCode::UNAUTHORIZED {
            let token = {
                let mut flow = self.flow.lock().await;
                {
                    let mut gate = self.gate();
                    if !gate.accepts(origin) { return Err(CHANGED.into()); }
                    self.consistent(&flow, &mut gate)?;
                }
                let owned = flow.session.as_ref().ok_or(CHANGED)?;
                if owned.revision == sent_revision {
                    self.refresh_locked(&mut flow, origin).await?
                } else {
                    owned.value.access_token.clone()
                }
            };
            #[cfg(test)]
            if let Some(io) = &self.test_io { io.pause(TestPoint::RetryAdmission).await; }
            response = self.admit_http(HttpOwner::Session(origin), route,
                Some(token), original_body)?.await?;
        }
        // Parsing happened inside the same awaited transport continuation; the
        // final gate also fences results delayed during body decoding.
        if !response.status.is_success() {
            let gate = self.gate();
            if !gate.accepts(origin) { return Err(CHANGED.into()); }
            return Err(format!("AceTeam request failed ({})", response.status.as_u16()));
        }
        self.finish_bound(origin, response.body)
    }
    pub(crate) fn publish_auth(&self, outcome: &AuthOutcome, publish: impl FnOnce(&AuthOutcome))
        -> bool
    {
        let gate = self.gate();
        if outcome.superseded || outcome.view.auth_epoch != gate.view.auth_epoch
            || outcome.view.view_revision != gate.view.view_revision { return false; }
        publish(outcome);
        true
    }
}


impl AuthState {
    fn reserve_control(&self, kind: ControlKind, expected: &str, operation: &str,
        target: Option<&str>) -> Result<Reservation, String>
    {
        let expected = parse_epoch(expected, false)?;
        if !valid_operation_id(operation) { return Err(CHANGED.into()); }
        let mut gate = self.gate();
        if matches!(gate.phase, Phase::Uninitialized | Phase::Disabled | Phase::Closing) {
            return Err(CHANGED.into());
        }
        let departing_receipt = gate.receipt.clone();
        let departing_lineage = gate.lineage.or(gate.cleanup_lineage);
        match kind {
            ControlKind::Begin => {
                if gate.epoch != expected || gate.phase == Phase::Session { return Err(CHANGED.into()); }
            }
            ControlKind::Logout => {
                if gate.epoch != expected { return Err(CHANGED.into()); }
            }
            ControlKind::Cancel => {
                let target = target.filter(|value| valid_operation_id(value)).ok_or(CHANGED)?;
                let receipt = gate.receipt.as_ref().ok_or(CHANGED)?;
                let exact = receipt.caller_epoch == expected && receipt.operation_id == target;
                let pending = receipt.pending_epoch == gate.epoch
                    && matches!(gate.phase, Phase::Starting | Phase::Attempt | Phase::Exchanging | Phase::Mfa);
                let installed = gate.phase == Phase::Session
                    && receipt.installed_lineage == gate.lineage;
                if !exact || (!pending && !installed) { return Err(CHANGED.into()); }
            }
        }
        let overflow_cleanup = gate.epoch.checked_add(1).map_or(true, |e| e >= TERMINAL)
            || gate.revision.checked_add(1).map_or(true, |v| v >= TERMINAL);
        if overflow_cleanup && kind == ControlKind::Logout {
            let owner = Owner { kind, operation_id: operation.into(), caller_epoch: expected,
                reserved_epoch: TERMINAL };
            // This exact pre-increment validation is the proof. Merely finding
            // Disabled later never grants the same cleanup authority.
            gate.owner = Some(owner.clone());
            gate.disable();
            return Ok(Reservation { owner, initial_view: gate.view.clone(),
                departing_receipt, departing_lineage, overflow_cleanup: true });
        }
        gate.advance(true)?;
        let owner = Owner { kind, operation_id: operation.into(),
            caller_epoch: expected, reserved_epoch: gate.epoch };
        gate.owner = Some(owner.clone());
        gate.receipt = if kind == ControlKind::Begin {
            Some(LoginReceipt { caller_epoch: expected, pending_epoch: gate.epoch,
                operation_id: operation.into(), installed_lineage: None })
        } else { None };
        gate.signed_out(if kind == ControlKind::Begin { Phase::Starting } else { Phase::Closing });
        Ok(Reservation { owner, initial_view: gate.view.clone(),
            departing_receipt, departing_lineage, overflow_cleanup: false })
    }
    fn outcome(&self, reservation: &Reservation, error: Option<String>) -> AuthOutcome {
        let gate = self.gate();
        let current = gate.owns(&reservation.owner);
        let terminal = gate.phase == Phase::Disabled
            && gate.terminal_owner.as_ref() == Some(&reservation.owner);
        AuthOutcome {
            operation_id: reservation.owner.operation_id.clone(),
            originating_auth_epoch: reservation.owner.caller_epoch.to_string(),
            reserved_auth_epoch: (!reservation.overflow_cleanup)
                .then(|| reservation.owner.reserved_epoch.to_string()),
            view: if current || terminal { gate.view.clone() } else { reservation.initial_view.clone() },
            error: if terminal { Some(error.unwrap_or_else(|| CLEANUP_UNCONFIRMED.into())) }
                else if current { error } else { Some(CHANGED.into()) },
            superseded: !(current || terminal),
        }
    }
    fn finish_signed_out(&self, reservation: &Reservation, error: Option<String>) -> AuthOutcome {
        let mut gate = self.gate();
        if !gate.owns(&reservation.owner) {
            drop(gate);
            return self.outcome(reservation, Some(CHANGED.into()));
        }
        if gate.advance(false).is_err() {
            return AuthOutcome {
                operation_id: reservation.owner.operation_id.clone(),
                originating_auth_epoch: reservation.owner.caller_epoch.to_string(),
                reserved_auth_epoch: Some(reservation.owner.reserved_epoch.to_string()),
                view: gate.view.clone(), error: Some(UNAVAILABLE.into()), superseded: false,
            };
        }
        gate.receipt = None;
        gate.signed_out(Phase::SignedOut);
        let outcome = AuthOutcome {
            operation_id: reservation.owner.operation_id.clone(),
            originating_auth_epoch: reservation.owner.caller_epoch.to_string(),
            reserved_auth_epoch: Some(reservation.owner.reserved_epoch.to_string()),
            view: gate.view.clone(), error, superseded: false,
        };
        gate.owner = None;
        outcome
    }
    async fn clean_pending(&self, flow: &mut AuthFlow) -> Result<(), String> {
        let mut failed = false;
        if let Some(chain) = &mut flow.cleanup {
            if chain.unresolved_issuance() { flow.unknown_issuance = true; }
            // Direct exchange delivered this chain's exact original session.
            // Its token custody, not a handle ACK, now owns cleanup. Hidden MFA
            // credentials deliberately do not gain that coverage assumption.
            if !(chain.credential_received && !chain.hidden_issued) {
                for (field, slot) in [("pending_id", &mut chain.pending), ("state", &mut chain.state)] {
                    if let Some(handle) = slot {
                        if !handle.acknowledged {
                            if self.cancel_exact(field, &handle.value).await.is_ok() {
                                handle.acknowledged = true;
                            } else { failed = true; }
                        }
                    }
                }
            }
        }
        flow.mfa = None;
        flow.attempt = None;
        if failed || flow.unknown_issuance { Err(CLEANUP_UNCONFIRMED.into()) } else { Ok(()) }
    }
    // Caller owns the flow and either an exact control reservation/target or
    // explicit already-Disabled cleanup. No current credential lookup occurs.
    async fn clean_owned(&self, flow: &mut AuthFlow) -> Result<(), String> {
        let mut failed = self.clean_pending(flow).await.is_err();
        let current = flow.session.as_ref().map(|s| s.value.clone());
        let candidate = flow.uncertain_session.clone();
        if current.is_some() && current == candidate {
            let acknowledged = flow.session_revoked || flow.candidate_revoked;
            flow.session_revoked = acknowledged;
            flow.candidate_revoked = acknowledged;
        }
        {
            let _gate = self.gate();
            match self.read_store() {
                Ok(None) => flow.store_cleared = true,
                Ok(Some(stored)) if current.as_ref() == Some(&stored)
                    || candidate.as_ref() == Some(&stored) => {
                    if self.delete_store().is_ok() { flow.store_cleared = true; }
                    else { failed = true; }
                }
                _ => failed = true, // Foreign entry is never deleted/adopted.
            }
        }
        if let Some(session) = &current {
            if !flow.session_revoked {
                if self.revoke_exact(&session.access_token).await.is_ok() { flow.session_revoked = true; }
                else { failed = true; }
            }
        }
        if let Some(session) = &candidate {
            if current.as_ref() == Some(session) && flow.session_revoked { flow.candidate_revoked = true; }
            if !flow.candidate_revoked {
                if self.revoke_exact(&session.access_token).await.is_ok() { flow.candidate_revoked = true; }
                else { failed = true; }
            }
        }
        // Acknowledge remote progress separately from local deletion. Failed
        // revocation must retain the exact token even when the store is empty.
        if flow.store_cleared {
            if flow.session_revoked { flow.session = None; }
            if flow.candidate_revoked {
                flow.uncertain_session = None;
                flow.candidate_origin = None;
            }
        }
        if failed || flow.unknown_issuance { Err(CLEANUP_UNCONFIRMED.into()) }
        else {
            flow.cleanup = None;
            flow.login_receipt = None;
            if flow.session.is_none() { self.gate().cleanup_lineage = None; }
            Ok(())
        }
    }
    fn candidate_capacity(&self, flow: &AuthFlow) -> Result<(), String> {
        if flow.uncertain_session.is_some() || flow.candidate_origin.is_some() || flow.unknown_issuance {
            self.gate().disable();
            Err(CLEANUP_UNCONFIRMED.into())
        } else { Ok(()) }
    }
    async fn abandon_login(&self, flow: &mut AuthFlow, reservation: &Reservation,
        error: String) -> AuthOutcome
    {
        if self.clean_pending(flow).await.is_err() {
            self.gate().disable();
            return self.outcome(reservation, Some(CLEANUP_UNCONFIRMED.into()));
        }
        flow.cleanup = None;
        self.finish_signed_out(reservation, Some(error))
    }
    pub(crate) async fn begin_login(&self, expected: &str, operation: &str)
        -> Result<BegunLogin, String>
    {
        let reservation = self.reserve_control(ControlKind::Begin, expected, operation, None)?;
        #[cfg(test)]
        if let Some(io) = &self.test_io { io.pause(TestPoint::BeginReserved).await; }
        let mut flow = self.flow.lock().await;
        if !self.gate().owns(&reservation.owner) {
            return Ok(BegunLogin { outcome: self.outcome(&reservation, None),
                owner: reservation, url: None });
        }
        if let Err(error) = self.clean_owned(&mut flow).await {
            self.gate().disable();
            let outcome = self.outcome(&reservation, Some(error));
            return Ok(BegunLogin { outcome, owner: reservation, url: None });
        }
        {
            let mut gate = self.gate();
            if !gate.owns(&reservation.owner) {
                return Ok(BegunLogin { outcome: self.outcome_without_lock(&gate, &reservation),
                    owner: reservation, url: None });
            }
            self.consistent(&flow, &mut gate)?;
            flow.login_receipt = gate.receipt.clone();
            if flow.cleanup.is_some() || flow.session.is_some() || flow.uncertain_session.is_some() {
                gate.disable();
                return Ok(BegunLogin { outcome: self.outcome_without_lock(&gate, &reservation),
                    owner: reservation, url: None });
            }
            flow.cleanup = flow.login_receipt.clone().map(LoginCleanup::new);
        }
        let verifier = match new_verifier() {
            Ok(verifier) => verifier,
            Err(error) => {
                let outcome = self.finish_signed_out(&reservation, Some(error));
                return Ok(BegunLogin { outcome, owner: reservation, url: None });
            }
        };
        let challenge = URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()));
        let pending = self.admit_http(HttpOwner::Control(&reservation.owner), Route::Start,
            None, json!({ "code_challenge": challenge }));
        let response = match pending {
            Ok(pending) => pending.await,
            Err(_) => return Ok(BegunLogin { outcome: self.outcome(&reservation, None),
                owner: reservation, url: None }),
        };
        let response = match response {
            Ok(response) if response.status.is_success() => response,
            Ok(_) => {
                flow.unknown_issuance = true; // Unknown initiation; no invented handle.
                let outcome = self.abandon_login(&mut flow, &reservation,
                    "Could not start AceTeam sign in".into()).await;
                return Ok(BegunLogin { outcome, owner: reservation, url: None });
            }
            Err(error) => {
                flow.unknown_issuance = true;
                let outcome = self.abandon_login(&mut flow, &reservation, error).await;
                return Ok(BegunLogin { outcome, owner: reservation, url: None });
            }
        };
        let identity = serde_json::from_value::<StartIdentity>(response.body.clone()).ok()
            .filter(|identity| valid_hex(&identity.state));
        if let Some(identity) = identity {
            let chain = flow.cleanup.as_mut().ok_or(CHANGED)?;
            if chain.receipt.operation_id != reservation.owner.operation_id
                || chain.receipt.pending_epoch != reservation.owner.reserved_epoch
                || chain.state.is_some() {
                self.gate().disable();
                return Err(CLEANUP_UNCONFIRMED.into());
            }
            chain.state = Some(CleanupHandle { value: identity.state, acknowledged: false });
        } else {
            flow.unknown_issuance = true;
        }
        let started = serde_json::from_value::<StartResponse>(response.body)
            .map_err(|_| "AceTeam returned an invalid sign-in link".to_string());
        let started = match started {
            Ok(started) if valid_hex(&started.state) && started.expires_in > 0
                && validate_login_url(&started.login_url, &started.state).is_ok() => started,
            _ => {
                let outcome = self.abandon_login(&mut flow, &reservation,
                    "AceTeam returned an invalid sign-in link".into()).await;
                return Ok(BegunLogin { outcome, owner: reservation, url: None });
            }
        };
        let accepted = {
            let mut gate = self.gate();
            if !gate.owns(&reservation.owner) { false }
            else if gate.advance(false).is_err() { false }
            else {
                flow.attempt = Some(LoginAttempt { state: started.state.clone(), verifier: verifier.clone(),
                    expires_at: Instant::now() + Duration::from_secs(started.expires_in.min(300)) });
                gate.signed_out(Phase::Attempt);
                true
            }
        };
        if !accepted {
            if self.clean_pending(&mut flow).await.is_err() {
                self.gate().disable();
            }
            return Ok(BegunLogin { outcome: self.outcome(&reservation, None),
                owner: reservation, url: None });
        }
        Ok(BegunLogin { outcome: self.outcome(&reservation, None),
            owner: reservation, url: Some(started.login_url) })
    }
    fn outcome_without_lock(&self, gate: &Authority, reservation: &Reservation) -> AuthOutcome {
        let current = gate.owns(&reservation.owner);
        let terminal = gate.phase == Phase::Disabled
            && gate.terminal_owner.as_ref() == Some(&reservation.owner);
        AuthOutcome {
            operation_id: reservation.owner.operation_id.clone(),
            originating_auth_epoch: reservation.owner.caller_epoch.to_string(),
            reserved_auth_epoch: (!reservation.overflow_cleanup)
                .then(|| reservation.owner.reserved_epoch.to_string()),
            view: if current || terminal { gate.view.clone() } else { reservation.initial_view.clone() },
            error: Some(if terminal { CLEANUP_UNCONFIRMED } else { CHANGED }.into()),
            superseded: !(current || terminal),
        }
    }
    pub(crate) async fn open_begin(&self, begun: BegunLogin,
        open: impl FnOnce(&str) -> Result<(), ()>) -> AuthOutcome
    {
        if begun.url.is_none() || begun.outcome.superseded || begun.outcome.error.is_some() {
            return begun.outcome;
        }
        let mut flow = self.flow.lock().await;
        let opened = {
            let gate = self.gate();
            if !gate.owns(&begun.owner.owner)
                || gate.view.auth_epoch != begun.outcome.view.auth_epoch
                || gate.view.view_revision != begun.outcome.view.view_revision {
                None
            } else if let Some(chain) = flow.cleanup.as_mut().filter(|chain| {
                chain.receipt.operation_id == begun.owner.owner.operation_id
                    && chain.receipt.pending_epoch == begun.owner.owner.reserved_epoch
            }) {
                // OS handoff itself may expose a mintable login state, even if
                // open subsequently reports an error or no callback arrives.
                chain.browser_exposed = true;
                Some(open(begun.url.as_deref().unwrap_or_default()))
            } else { None }
        };
        drop(flow);
        match opened {
            Some(Ok(())) => begun.outcome,
            None => self.outcome(&begun.owner, Some(CHANGED.into())),
            Some(Err(())) => {
                // Exact predecessor receipt, never cancel whichever login is current.
                let _cleanup = self.cancel_login(
                    &begun.owner.owner.caller_epoch.to_string(),
                    &begun.owner.owner.operation_id,
                    &begun.owner.owner.operation_id).await;
                // Cancellation owns its own reservation and cleanup progress.
                // Never relabel its current/terminal view as the old BEGIN.
                // The frontend reconciles fresh session_status after this
                // original-owner outcome, including a superseded one.
                self.outcome(&begun.owner, Some("Could not open the browser for sign in".into()))
            }
        }
    }
    async fn close_reserved(&self, reservation: Reservation) -> AuthOutcome {
        let mut flow = self.flow.lock().await;
        let live = {
            let gate = self.gate();
            if !gate.owns(&reservation.owner)
                && !(gate.phase == Phase::Disabled && gate.terminal_owner.as_ref() == Some(&reservation.owner)) {
                drop(gate);
                return self.outcome(&reservation, None);
            }
            gate.owns(&reservation.owner)
        };
        let receipt_matches = |receipt: &LoginReceipt| reservation.departing_receipt.as_ref()
            .is_some_and(|target| receipt.caller_epoch == target.caller_epoch
                && receipt.pending_epoch == target.pending_epoch && receipt.operation_id == target.operation_id);
        let exact = flow.session.as_ref().map_or(true, |session|
            Some(session.lineage) == reservation.departing_lineage)
            && flow.cleanup.as_ref().map_or(true, |chain| receipt_matches(&chain.receipt))
            && flow.candidate_origin.as_ref().map_or(true, |origin| match origin {
                CandidateOrigin::Lineage(lineage) => Some(*lineage) == reservation.departing_lineage,
                CandidateOrigin::Login(receipt) => receipt_matches(receipt),
            });
        if !exact {
            self.gate().disable();
            return self.outcome(&reservation, Some(CLEANUP_UNCONFIRMED.into()));
        }
        let mut error = self.clean_pending(&mut flow).await.err();
        let departing = flow.session.as_ref()
            .filter(|s| Some(s.lineage) == reservation.departing_lineage)
            .map(|s| s.value.clone());
        // Normal departing-session refresh is cleanup-only and never saved or
        // made available to application requests.
        if let Some(session) = &departing {
            if live && error.is_none() && session.expires_at <= now_seconds().saturating_add(60)
                && self.candidate_capacity(&flow).is_ok() {
                let consistent = {
                    let mut gate = self.gate();
                    gate.owns(&reservation.owner) && self.consistent(&flow, &mut gate).is_ok()
                };
                if !consistent {
                    let _ = self.clean_owned(&mut flow).await;
                    return self.outcome(&reservation, Some(CLEANUP_UNCONFIRMED.into()));
                }
                let mut body = json!({ "refresh_token": session.refresh_token });
                if let Some(device_id) = &session.device_id { body["device_id"] = json!(device_id); }
                match self.admit_http(HttpOwner::Control(&reservation.owner),
                    Route::Refresh, None, body) {
                    Ok(pending) => match pending.await {
                        Ok(response) if response.status.is_success() => {
                            if let Ok(payload) = serde_json::from_value::<TokenResponse>(response.body) {
                                if validate_tokens(&payload).is_ok() {
                                    flow.uncertain_session = Some(Session::from_response(payload, Some(session.clone())));
                                    flow.candidate_origin = reservation.departing_lineage.map(CandidateOrigin::Lineage);
                                    flow.candidate_revoked = false;
                                } else { flow.unknown_issuance = true; }
                            } else { flow.unknown_issuance = true; }
                        }
                        _ => flow.unknown_issuance = true,
                    },
                    Err(_) => return self.outcome(&reservation, None),
                }
                if flow.unknown_issuance {
                    flow.unknown_refresh_origin = reservation.departing_lineage.map(|lineage|
                        AuthOrigin { epoch: reservation.owner.caller_epoch, lineage });
                }
            }
        }
        if let Err(cleanup_error) = self.clean_owned(&mut flow).await { error = Some(cleanup_error); }
        if error.is_some() {
            self.gate().disable();
            return self.outcome(&reservation, Some(CLEANUP_UNCONFIRMED.into()));
        }
        if !live { return self.outcome(&reservation, Some(UNAVAILABLE.into())); }
        self.finish_signed_out(&reservation, error)
    }
    pub async fn cancel_login(&self, expected: &str, operation: &str, target: &str)
        -> Result<AuthOutcome, String>
    {
        let reservation = self.reserve_control(ControlKind::Cancel, expected, operation, Some(target))?;
        Ok(self.close_reserved(reservation).await)
    }
    pub async fn sign_out(&self, expected: &str, operation: &str) -> Result<AuthOutcome, String> {
        if parse_epoch(expected, true)? == TERMINAL {
            if !valid_operation_id(operation) { return Err(CHANGED.into()); }
            return self.cleanup_disabled(operation).await;
        }
        let reservation = self.reserve_control(ControlKind::Logout, expected, operation, None)?;
        Ok(self.close_reserved(reservation).await)
    }
    async fn cleanup_disabled(&self, operation: &str) -> Result<AuthOutcome, String> {
        let mut flow = self.flow.lock().await;
        if self.gate().phase != Phase::Disabled { return Err(CHANGED.into()); }
        let error = self.clean_owned(&mut flow).await.err();
        Ok(AuthOutcome {
            operation_id: operation.into(), originating_auth_epoch: TERMINAL.to_string(),
            reserved_auth_epoch: None, view: AuthView::empty(TERMINAL, TERMINAL),
            error, superseded: false,
        })
    }
}


impl AuthState {
    pub async fn exchange_callback(&self, callback: &str) -> Result<AuthOutcome, String> {
        // Parsing/mismatched state cannot consume or cancel another attempt.
        let (code, state) = callback_values(callback)?;
        let mut flow = self.flow.lock().await;
        let (reservation, attempt, chain) = {
            let mut gate = self.gate();
            let owner = gate.owner.clone().filter(|o| o.kind == ControlKind::Begin)
                .ok_or(CHANGED)?;
            if !gate.owns(&owner) || gate.phase != Phase::Attempt { return Err(CHANGED.into()); }
            let attempt = flow.attempt.as_ref().ok_or(CHANGED)?;
            if attempt.state != state { return Err("This sign-in link is no longer active".into()); }
            if Instant::now() >= attempt.expires_at { return Err("This sign-in link expired".into()); }
            self.consistent(&flow, &mut gate)?;
            if flow.uncertain_session.is_some() || flow.candidate_origin.is_some() || flow.unknown_issuance {
                gate.disable();
                return Err(CLEANUP_UNCONFIRMED.into());
            }
            let reservation = Reservation { owner, initial_view: gate.view.clone(),
                departing_receipt: None, departing_lineage: None, overflow_cleanup: false };
            let chain = flow.cleanup.as_ref().filter(|chain| {
                chain.receipt.operation_id == reservation.owner.operation_id
                    && chain.receipt.pending_epoch == reservation.owner.reserved_epoch
            }).ok_or(CHANGED)?.receipt.clone();
            gate.advance(false)?;
            gate.signed_out(Phase::Exchanging);
            let attempt = flow.attempt.take().ok_or(CHANGED)?;
            (reservation, attempt, chain)
        };
        let result = match self.admit_http(HttpOwner::Control(&reservation.owner), Route::Exchange,
            None, json!({ "code": code, "state": state, "code_verifier": attempt.verifier })) {
                Ok(pending) => {
                    flow.cleanup.as_mut().ok_or(CHANGED)?.exchange_admitted = true;
                    pending.await
                }
                Err(_) => return Ok(self.outcome(&reservation, Some(CHANGED.into()))),
            };
        self.finish_login_response(&mut flow, reservation, chain, result, true).await
    }
    pub async fn verify_mfa(&self, expected: &str, operation: &str, factor_id: &str, code: &str)
        -> Result<AuthOutcome, String>
    {
        let expected_number = parse_epoch(expected, false)?;
        if !valid_operation_id(operation) { return Err(CHANGED.into()); }
        let mut flow = self.flow.lock().await;
        let (reservation, pending_id, chain) = {
            let mut gate = self.gate();
            let owner = gate.owner.clone().filter(|o| o.kind == ControlKind::Begin)
                .ok_or(CHANGED)?;
            if gate.epoch != expected_number || gate.phase != Phase::Mfa || !gate.owns(&owner) {
                return Err(CHANGED.into());
            }
            let pending = flow.mfa.as_ref().ok_or("No verification is pending")?;
            if Instant::now() >= pending.expires_at {
                return Err("Verification expired. Please start sign in again".into());
            }
            if !pending.factors.iter().any(|f| f.id == factor_id)
                || !(6..=8).contains(&code.len()) || !code.bytes().all(|b| b.is_ascii_digit()) {
                return Err("Choose a factor and enter its verification code".into());
            }
            self.consistent(&flow, &mut gate)?;
            if flow.uncertain_session.is_some() || flow.candidate_origin.is_some() || flow.unknown_issuance {
                gate.disable();
                return Err(CLEANUP_UNCONFIRMED.into());
            }
            let reservation = Reservation { owner, initial_view: gate.view.clone(),
                departing_receipt: None, departing_lineage: None, overflow_cleanup: false };
            let chain = flow.cleanup.as_ref().filter(|chain| {
                chain.receipt.operation_id == reservation.owner.operation_id
                    && chain.receipt.pending_epoch == reservation.owner.reserved_epoch
            }).ok_or(CHANGED)?.receipt.clone();
            gate.advance(false)?;
            gate.signed_out(Phase::Exchanging);
            let pending_id = flow.mfa.take().ok_or(CHANGED)?.id;
            (reservation, pending_id, chain)
        };
        let result = match self.admit_http(HttpOwner::Control(&reservation.owner), Route::Mfa,
            None, json!({ "pending_id": pending_id, "factor_id": factor_id, "code": code })) {
                Ok(pending) => {
                    flow.cleanup.as_mut().ok_or(CHANGED)?.mfa_admitted = true;
                    pending.await
                }
                Err(_) => return Ok(self.outcome(&reservation, Some(CHANGED.into()))),
            };
        let mut outcome = self.finish_login_response(&mut flow, reservation, chain, result, false).await?;
        outcome.operation_id = operation.into();
        outcome.originating_auth_epoch = expected.into();
        outcome.reserved_auth_epoch = None;
        Ok(outcome)
    }
    async fn finish_login_response(&self, flow: &mut AuthFlow, reservation: Reservation, chain: LoginReceipt,
        result: Result<WireResponse, String>, allow_mfa: bool) -> Result<AuthOutcome, String>
    {
        if !flow.cleanup.as_ref().is_some_and(|owned| owned.receipt == chain) {
            self.gate().disable();
            return Ok(self.outcome(&reservation, Some(CLEANUP_UNCONFIRMED.into())));
        }
        let response = match result {
            Ok(response) => response,
            Err(error) => return Ok(self.abandon_login(flow, &reservation, error).await),
        };
        if allow_mfa && response.status == StatusCode::ACCEPTED {
            let identity = serde_json::from_value::<PendingIdentity>(response.body.clone()).ok()
                .filter(|identity| identity.mfa_required && valid_hex(&identity.pending_id));
            if let Some(identity) = identity {
                let owned = flow.cleanup.as_mut().ok_or(CHANGED)?;
                if owned.pending.is_some() {
                    flow.unknown_issuance = true;
                    self.gate().disable();
                    return Ok(self.outcome(&reservation, Some(CLEANUP_UNCONFIRMED.into())));
                }
                owned.pending = Some(CleanupHandle { value: identity.pending_id, acknowledged: false });
                owned.hidden_issued = true;
            }
            let pending: MfaPendingResponse = match serde_json::from_value(response.body) {
                Ok(pending) => pending,
                Err(_) => return Ok(self.abandon_login(flow, &reservation,
                    "AceTeam returned an invalid verification request".into()).await),
            };
            if !pending.mfa_required || !valid_hex(&pending.pending_id) || !valid_factors(&pending.factors) {
                return Ok(self.abandon_login(flow, &reservation,
                    "AceTeam returned an invalid verification request".into()).await);
            }
            let accepted = {
                let mut gate = self.gate();
                if !gate.owns(&reservation.owner) { false }
                else if self.consistent(flow, &mut gate).is_err() { false }
                else if gate.advance(false).is_err() { false }
                else {
                    gate.phase = Phase::Mfa;
                    gate.view.mfa_pending = true;
                    gate.view.factors = pending.factors.clone();
                    flow.mfa = Some(PendingMfa { id: pending.pending_id.clone(),
                        factors: pending.factors.clone(), expires_at: Instant::now() + MFA_LIFETIME });
                    true
                }
            };
            if !accepted {
                return Ok(self.abandon_login(flow, &reservation, CHANGED.into()).await);
            }
            return Ok(self.outcome(&reservation, None));
        }
        if !response.status.is_success() {
            return Ok(self.abandon_login(flow, &reservation,
                "Sign in failed. Please start again".into()).await);
        }
        let payload: TokenResponse = match serde_json::from_value(response.body) {
            Ok(payload) => payload,
            Err(_) => return Ok(self.abandon_login(flow, &reservation,
                "AceTeam returned an invalid session".into()).await),
        };
        if let Err(error) = validate_tokens(&payload) {
            return Ok(self.abandon_login(flow, &reservation, error).await);
        }
        let session = Session::from_response(payload, None);
        self.candidate_capacity(flow)?;
        flow.uncertain_session = Some(session.clone());
        flow.candidate_origin = Some(CandidateOrigin::Login(chain));
        flow.candidate_revoked = false;
        flow.cleanup.as_mut().ok_or(CHANGED)?.credential_received = true;
        #[cfg(test)]
        if let Some(io) = &self.test_io { io.pause(TestPoint::LoginCommit).await; }
        let committed = {
            let mut gate = self.gate();
            if !gate.owns(&reservation.owner) { None }
            else if self.consistent(flow, &mut gate).is_err() { None }
            else if gate.advance(true).is_err() { None }
            else {
                let lineage = gate.epoch;
                flow.store_cleared = false;
                if self.write_store(&session).is_err() {
                    gate.disable();
                    None
                } else {
                    flow.uncertain_session = None;
                    flow.candidate_origin = None;
                    flow.session = Some(OwnedSession { lineage, revision: 0, value: session.clone() });
                    flow.store_cleared = false;
                    flow.session_revoked = false;
                    if let Some(receipt) = &mut flow.login_receipt {
                        receipt.installed_lineage = Some(lineage);
                    }
                    gate.receipt = flow.login_receipt.clone();
                    gate.owner = None;
                    gate.phase = Phase::Session;
                    gate.lineage = Some(lineage);
                    gate.cleanup_lineage = None;
                    gate.view.signed_in = true;
                    gate.view.email = session.email.clone();
                    gate.view.mfa_pending = false;
                    gate.view.factors.clear();
                    gate.view.cancel_target = gate.receipt.as_ref().map(LoginCancelTarget::from);
                    Some(AuthOutcome {
                        operation_id: reservation.owner.operation_id.clone(),
                        originating_auth_epoch: reservation.owner.caller_epoch.to_string(),
                        reserved_auth_epoch: Some(reservation.owner.reserved_epoch.to_string()),
                        view: gate.view.clone(), error: None, superseded: false,
                    })
                }
            }
        };
        if let Some(outcome) = committed { return Ok(outcome); }
        if let Err(error) = self.revoke_exact(&session.access_token).await {
            self.gate().disable();
            return Ok(self.outcome(&reservation, Some(error)));
        }
        flow.candidate_revoked = true;
        Ok(self.outcome(&reservation, Some(CHANGED.into())))
    }
}

fn parse_epoch(value: &str, terminal: bool) -> Result<u64, String> {
    if value.is_empty() || value.len() > 20 || value.starts_with('0')
        || !value.bytes().all(|b| b.is_ascii_digit()) { return Err(CHANGED.into()); }
    let number = value.parse::<u64>().map_err(|_| CHANGED.to_string())?;
    if number == 0 || (!terminal && number == TERMINAL) { Err(CHANGED.into()) } else { Ok(number) }
}
pub(crate) fn valid_operation_id(value: &str) -> bool {
    let b = value.as_bytes();
    b.len() == 36 && b[14] == b'4' && matches!(b[19], b'8' | b'9' | b'a' | b'b')
        && b.iter().enumerate().all(|(i, c)| {
            if matches!(i, 8 | 13 | 18 | 23) { *c == b'-' }
            else { c.is_ascii_digit() || (b'a'..=b'f').contains(c) }
        })
}

#[derive(Deserialize)]
struct StartResponse {
    state: String,
    login_url: String,
    expires_in: u64,
}
#[derive(Deserialize)]
struct StartIdentity { state: String }
#[derive(Deserialize)]
struct PendingIdentity { mfa_required: bool, pending_id: String }

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

fn valid_factors(factors: &[MfaFactor]) -> bool {
    !factors.is_empty() && factors.iter().enumerate().all(|(index, factor)| {
        !factor.id.is_empty() && !factors[..index].iter().any(|prior| prior.id == factor.id)
    })
}

#[derive(Clone, Serialize, Deserialize, PartialEq, Eq)]
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
    value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn take_attempt(flow: &mut AuthFlow, state: &str) -> Result<LoginAttempt, Option<LoginAttempt>> {
    let matches = flow.attempt.as_ref().is_some_and(|attempt| {
        attempt.state == state && Instant::now() < attempt.expires_at
    });
    if matches { flow.attempt.take().ok_or(None) } else { Err(None) }
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
struct SentRequest { route: Route, method: Method, token: Option<String>, body: Value }
#[cfg(test)]
#[derive(Clone, Copy, PartialEq, Eq)]
enum TestPoint { BeginReserved, OriginalAdmission, RetryAdmission, RefreshAdmission, RefreshCommit, LoginCommit }
#[cfg(test)]
#[derive(Clone, Copy, PartialEq, Eq)]
enum StorePoint { Write, Delete }
#[cfg(test)]
pub(crate) struct TestIo {
    stored: AuthorityMutex<Option<Session>>,
    reads: std::sync::atomic::AtomicUsize,
    writes: std::sync::atomic::AtomicUsize,
    deletes: std::sync::atomic::AtomicUsize,
    fail_read: std::sync::atomic::AtomicBool,
    fail_write: std::sync::atomic::AtomicBool,
    fail_after_write: std::sync::atomic::AtomicBool,
    fail_delete: std::sync::atomic::AtomicBool,
    fail_after_delete: std::sync::atomic::AtomicBool,
    pauses: AuthorityMutex<std::collections::VecDeque<(TestPoint,
        tokio::sync::oneshot::Sender<()>, tokio::sync::oneshot::Receiver<()>)>>,
    store_pause: AuthorityMutex<Option<(StorePoint, std::sync::mpsc::Sender<()>,
        std::sync::mpsc::Receiver<()>)>>,
    scripts: AuthorityMutex<std::collections::VecDeque<(Route, tokio::sync::oneshot::Receiver<RawResponse>)>>,
    sent: AuthorityMutex<Vec<SentRequest>>,
    sent_count: tokio::sync::watch::Sender<usize>,
}
#[cfg(test)]
impl TestIo {
    fn new(stored: Option<Session>) -> Self {
        Self {
            stored: AuthorityMutex::new(stored),
            reads: std::sync::atomic::AtomicUsize::new(0),
            writes: std::sync::atomic::AtomicUsize::new(0),
            deletes: std::sync::atomic::AtomicUsize::new(0),
            fail_read: std::sync::atomic::AtomicBool::new(false),
            fail_write: std::sync::atomic::AtomicBool::new(false),
            fail_after_write: std::sync::atomic::AtomicBool::new(false),
            fail_delete: std::sync::atomic::AtomicBool::new(false),
            fail_after_delete: std::sync::atomic::AtomicBool::new(false),
            pauses: AuthorityMutex::new(std::collections::VecDeque::new()),
            store_pause: AuthorityMutex::new(None),
            scripts: AuthorityMutex::new(std::collections::VecDeque::new()),
            sent: AuthorityMutex::new(vec![]),
            sent_count: tokio::sync::watch::channel(0).0,
        }
    }
    fn read(&self) -> Result<Option<Session>, String> {
        self.reads.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        if self.fail_read.load(std::sync::atomic::Ordering::SeqCst) { return Err("Fixture store read failure".into()); }
        Ok(self.stored.lock().map_err(|_| UNAVAILABLE)?.clone())
    }
    fn write(&self, session: &Session) -> Result<(), String> {
        self.pause_store(StorePoint::Write);
        self.writes.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        if self.fail_write.load(std::sync::atomic::Ordering::SeqCst) { return Err("Fixture store write failure".into()); }
        *self.stored.lock().map_err(|_| UNAVAILABLE)? = Some(session.clone());
        if self.fail_after_write.load(std::sync::atomic::Ordering::SeqCst) {
            return Err("Fixture store acknowledgement failure".into());
        }
        Ok(())
    }
    fn clear(&self) -> Result<(), String> {
        self.pause_store(StorePoint::Delete);
        self.deletes.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        if self.fail_delete.load(std::sync::atomic::Ordering::SeqCst) {
            return Err("Fixture store deletion failure".into());
        }
        *self.stored.lock().map_err(|_| UNAVAILABLE)? = None;
        if self.fail_after_delete.load(std::sync::atomic::Ordering::SeqCst) {
            return Err("Fixture store deletion acknowledgement failure".into());
        }
        Ok(())
    }
    fn pause_store(&self, point: StorePoint) {
        let pause = {
            let mut slot = self.store_pause.lock().unwrap();
            if slot.as_ref().is_some_and(|(expected, _, _)| *expected == point) { slot.take() }
            else { None }
        };
        if let Some((_, entered, release)) = pause {
            let _ = entered.send(());
            release.recv_timeout(Duration::from_secs(5)).expect("Fixture store barrier not released");
        }
    }
    fn hold_store(&self, point: StorePoint) -> (std::sync::mpsc::Receiver<()>, std::sync::mpsc::Sender<()>) {
        let (entered, observed) = std::sync::mpsc::channel();
        let (release, wait) = std::sync::mpsc::channel();
        assert!(self.store_pause.lock().unwrap().replace((point, entered, wait)).is_none());
        (observed, release)
    }
    fn hold(&self, point: TestPoint) -> (tokio::sync::oneshot::Receiver<()>, tokio::sync::oneshot::Sender<()>) {
        let (entered, observed) = tokio::sync::oneshot::channel();
        let (release, wait) = tokio::sync::oneshot::channel();
        self.pauses.lock().unwrap().push_back((point, entered, wait));
        (observed, release)
    }
    async fn pause(&self, point: TestPoint) {
        let pause = {
            let mut queue = self.pauses.lock().unwrap();
            if queue.front().is_some_and(|(expected, _, _)| *expected == point) { queue.pop_front() }
            else { None }
        };
        if let Some((_, entered, release)) = pause {
            let _ = entered.send(());
            tokio::time::timeout(Duration::from_secs(5), release).await
                .expect("Fixture admission barrier timed out").expect("Fixture admission barrier dropped");
        }
    }
    fn body(&self, route: Route, status: u16) -> (tokio::sync::oneshot::Receiver<()>,
        tokio::sync::oneshot::Sender<Result<Vec<u8>, String>>)
    {
        let response = self.defer(route);
        let (started, observed) = tokio::sync::oneshot::channel();
        let (body, pending) = tokio::sync::oneshot::channel();
        assert!(response.send(RawResponse::DeferredBody { status: StatusCode::from_u16(status).unwrap(),
            started, body: pending }).is_ok());
        (observed, body)
    }
    fn defer(&self, route: Route) -> tokio::sync::oneshot::Sender<RawResponse> {
        let (sender, receiver) = tokio::sync::oneshot::channel();
        self.scripts.lock().unwrap().push_back((route, receiver));
        sender
    }
    fn raw(&self, route: Route, status: u16, body: &[u8]) {
        let sender = self.defer(route);
        assert!(sender.send(RawResponse::Scripted {
            status: StatusCode::from_u16(status).unwrap(), body: body.into(),
        }).is_ok());
    }
    fn json(&self, route: Route, status: u16, body: Value) {
        self.raw(route, status, &serde_json::to_vec(&body).unwrap());
    }
    fn start(&self, route: Route, token: Option<String>, body: Value) -> PendingResponse {
        let count = {
            let mut sent = self.sent.lock().unwrap();
            sent.push(SentRequest { route, method: route.method(), token, body });
            sent.len()
        };
        self.sent_count.send_replace(count);
        let script = self.scripts.lock().unwrap().pop_front();
        match script {
            Some((expected, receiver)) if expected.path() == route.path() => Box::pin(async move {
                let response = receiver.await.map_err(|_| "Fixture response was not delivered".to_string())?;
                read_response(route, response).await
            }),
            _ => Box::pin(async { Err("Unexpected fixture request".into()) }),
        }
    }
    async fn await_count(&self, expected: usize) {
        let mut receiver = self.sent_count.subscribe();
        let wait = async {
            while *receiver.borrow_and_update() < expected {
                receiver.changed().await.expect("Fixture request observer closed");
            }
        };
        assert!(tokio::time::timeout(Duration::from_secs(5), wait).await.is_ok());
    }
    pub(crate) fn raw_logout(&self) { self.raw(Route::Logout, 204, b""); }
    pub(crate) fn queue_login(&self) {
        let state = "b".repeat(64);
        self.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
            "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") }));
        self.json(Route::Exchange, 200, json!({ "access_token": "fixture-access-B",
            "refresh_token": "fixture-refresh-B", "expires_in": 3600,
            "device_id": "fixture-device-B", "user": { "email": "account-b@example.invalid" } }));
    }
    pub(crate) fn request_count(&self) -> usize { self.sent.lock().unwrap().len() }
}
#[cfg(test)]
impl AuthState {
    fn fixture(stored: Option<Session>) -> (std::sync::Arc<Self>, std::sync::Arc<TestIo>) {
        let io = std::sync::Arc::new(TestIo::new(stored));
        let auth = std::sync::Arc::new(Self {
            flow: Mutex::new(AuthFlow::default()),
            authority: AuthorityMutex::new(Authority::default()),
            client: None,
            test_io: Some(io.clone()),
        });
        (auth, io)
    }
    pub(crate) fn signed_in_fixture() -> (std::sync::Arc<Self>, std::sync::Arc<TestIo>) {
        Self::fixture(Some(Session {
            access_token: "fixture-access-A".into(), refresh_token: "fixture-refresh-A".into(),
            expires_at: now_seconds() + 3600, device_id: Some("fixture-device-A".into()),
            email: Some("account-a@example.invalid".into()),
        }))
    }
}
#[cfg(test)]
mod continuation_tests {
    use super::*;
    use std::sync::atomic::Ordering;
    const OP1: &str = "11111111-1111-4111-8111-111111111111";
    const OP2: &str = "22222222-2222-4222-8222-222222222222";
    const OP3: &str = "33333333-3333-4333-8333-333333333333";
    fn tokens(account: &str) -> Value {
        json!({ "access_token": format!("fixture-access-{account}"),
            "refresh_token": format!("fixture-refresh-{account}"), "expires_in": 3600,
            "device_id": format!("fixture-device-{account}"),
            "user": { "email": format!("account-{account}@example.invalid") } })
    }
    async fn login(auth: &AuthState, io: &TestIo, expected: &str, operation: &str) -> AuthOutcome {
        let state = "b".repeat(64);
        io.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
            "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") }));
        let begun = auth.begin_login(expected, operation).await.unwrap();
        let opened = auth.open_begin(begun, |_| Ok(())).await;
        assert!(!opened.superseded && opened.error.is_none());
        io.json(Route::Exchange, 200, tokens("B"));
        auth.exchange_callback(&format!("citadel://auth/callback?code={}&state={state}", "a".repeat(64)))
            .await.unwrap()
    }
    #[test]
    fn bootstrap_is_once_and_owned_not_re_adopted() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            let first = auth.view().await.unwrap();
            assert!(first.signed_in && first.auth_epoch == "1");
            let reads = io.reads.load(Ordering::SeqCst);
            let next = auth.view().await.unwrap();
            assert!(next.auth_epoch == "1" && io.reads.load(Ordering::SeqCst) == reads + 1);
            io.stored.lock().unwrap().as_mut().unwrap().access_token = "unexpected-entry".into();
            let refused = auth.view().await.unwrap();
            assert!(refused.auth_epoch == TERMINAL.to_string() && !refused.signed_in);
            assert!(io.deletes.load(Ordering::SeqCst) == 0);
            let stopped_reads = io.reads.load(Ordering::SeqCst);
            assert!(auth.view().await.unwrap().auth_epoch == TERMINAL.to_string());
            assert!(io.reads.load(Ordering::SeqCst) == stopped_reads);
        });
    }
    #[test]
    fn failed_bootstrap_returns_terminal_view_without_read_retry() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            io.fail_read.store(true, Ordering::SeqCst);
            let view = auth.view().await.unwrap();
            assert!(view.auth_epoch == TERMINAL.to_string() && !view.signed_in);
            assert!(auth.view().await.unwrap().view_revision == TERMINAL.to_string());
            assert!(io.reads.load(Ordering::SeqCst) == 1);
            assert!(auth.begin_login("1", OP1).await.is_err());
        });
    }
    #[test]
    fn delayed_401_never_refreshes_or_retries_as_replacement() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            let response = io.defer(Route::Nodes);
            let pending_auth = auth.clone();
            let request = tauri::async_runtime::spawn(async move {
                pending_auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await
            });
            io.await_count(1).await;
            io.raw_logout();
            let logout = auth.sign_out("1", OP1).await.unwrap();
            let replacement = login(&auth, &io, &logout.view.auth_epoch, OP2).await;
            assert!(replacement.view.signed_in);
            assert!(response.send(RawResponse::Scripted {
                status: StatusCode::UNAUTHORIZED, body: b"not-json".to_vec() }).is_ok());
            assert!(request.await.unwrap().is_err());
            assert!(io.request_count() == 4);
            assert!(io.sent.lock().unwrap().iter().filter(|s| matches!(s.route, Route::Refresh)).count() == 0);
            assert!(auth.view().await.unwrap().auth_epoch == replacement.view.auth_epoch);
        });
    }
    #[test]
    fn delayed_mutating_response_is_not_a_new_accounts_result() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            let response = io.defer(Route::Approve);
            let task_auth = auth.clone();
            let request = tauri::async_runtime::spawn(async move {
                task_auth.request_bound(&origin, Method::POST, "/api/fabric/device-auth/approve",
                    Some(json!({ "user_code": "ABCD-1234" }))).await
            });
            io.await_count(1).await;
            io.raw_logout();
            auth.sign_out("1", OP1).await.unwrap();
            assert!(response.send(RawResponse::Scripted {
                status: StatusCode::OK, body: b"{}".to_vec() }).is_ok());
            assert!(request.await.unwrap().is_err());
            assert!(io.request_count() == 2);
            let sent = io.sent.lock().unwrap();
            assert!(sent[0].token.as_deref() == Some("fixture-access-A"));
            assert!(sent[0].body == json!({ "user_code": "ABCD-1234" }));
        });
    }
    #[test]
    fn same_lineage_refresh_retries_once_with_rotated_credential() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            io.raw(Route::Nodes, 401, b"");
            io.json(Route::Refresh, 200, tokens("rotated-A"));
            io.json(Route::Nodes, 200, json!([]));
            let value = auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.unwrap();
            assert!(value.auth_epoch == "1" && value.value == json!([]));
            let flow = auth.flow.lock().await;
            assert!(flow.session.as_ref().unwrap().lineage == origin.lineage);
            assert!(flow.session.as_ref().unwrap().revision == 1);
            let sent = io.sent.lock().unwrap();
            assert!(sent.len() == 3 && sent[2].token.as_deref() == Some("fixture-access-rotated-A"));
        });
    }
    #[test]
    fn wrong_callback_cannot_consume_the_actual_current_attempt() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            auth.view().await.unwrap();
            let state = "b".repeat(64);
            io.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
                "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") }));
            let begun = auth.begin_login("1", OP1).await.unwrap();
            auth.open_begin(begun, |_| Ok(())).await;
            assert!(auth.exchange_callback(&format!("citadel://auth/callback?code={}&state={}",
                "a".repeat(64), "c".repeat(64))).await.is_err());
            assert!(auth.exchange_callback("not-a-callback").await.is_err());
            assert!(io.request_count() == 1);
            {
                let flow = auth.flow.lock().await;
                assert!(flow.attempt.as_ref().is_some_and(|a| a.state == state));
            }
            io.json(Route::Exchange, 200, tokens("B"));
            let installed = auth.exchange_callback(&format!("citadel://auth/callback?code={}&state={state}",
                "a".repeat(64))).await.unwrap();
            assert!(installed.view.signed_in);
        });
    }
    #[test]
    fn non_json_refresh_401_still_clears_only_its_owned_session() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            io.raw(Route::Nodes, 401, b"");
            io.raw(Route::Refresh, 401, b"not-json");
            assert!(auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.is_err());
            assert!(io.deletes.load(Ordering::SeqCst) == 1);
            let view = auth.view().await.unwrap();
            assert!(!view.signed_in && view.auth_epoch == "2");
        });
    }
    #[test]
    fn empty_204_cleanup_and_exact_predecessor_cancel_are_accepted() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            auth.view().await.unwrap();
            let installed = login(&auth, &io, "1", OP1).await;
            assert!(installed.view.auth_epoch == "3");
            let target = installed.view.cancel_target.unwrap();
            assert!(target.expected_auth_epoch == "1" && target.operation_id == OP1);
            io.raw_logout();
            let cancelled = auth.cancel_login(&target.expected_auth_epoch, OP2, OP1).await.unwrap();
            assert!(!cancelled.view.signed_in && cancelled.error.is_none());
            let replacement = login(&auth, &io, &cancelled.view.auth_epoch, OP3).await;
            assert!(auth.cancel_login("1", OP2, OP1).await.is_err());
            assert!(auth.view().await.unwrap().auth_epoch == replacement.view.auth_epoch);
        });
    }
    #[test]
    fn old_auth_outcome_cannot_be_handed_to_the_emitter_after_logout() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            auth.view().await.unwrap();
            let installed = login(&auth, &io, "1", OP1).await;
            io.raw_logout();
            auth.sign_out(&installed.view.auth_epoch, OP2).await.unwrap();
            let emitted = std::sync::atomic::AtomicBool::new(false);
            assert!(!auth.publish_auth(&installed, |_| { emitted.store(true, Ordering::SeqCst); }));
            assert!(!emitted.load(Ordering::SeqCst));
        });
    }
    #[test]
    fn mfa_installs_a_new_lineage_and_preserves_only_its_exact_cancel_receipt() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            auth.view().await.unwrap();
            let state = "b".repeat(64);
            io.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
                "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") }));
            let begun = auth.begin_login("1", OP1).await.unwrap();
            auth.open_begin(begun, |_| Ok(())).await;
            io.json(Route::Exchange, 202, json!({ "mfa_required": true,
                "pending_id": "d".repeat(64), "factors": [{ "id": "factor-A", "friendly_name": null }] }));
            let pending = auth.exchange_callback(&format!("citadel://auth/callback?code={}&state={state}",
                "a".repeat(64))).await.unwrap();
            assert!(pending.view.mfa_pending && pending.view.auth_epoch == "2");
            let target = pending.view.cancel_target.unwrap();
            assert!(target.expected_auth_epoch == "1" && target.operation_id == OP1);
            io.json(Route::Mfa, 200, tokens("B"));
            let installed = auth.verify_mfa("2", OP2, "factor-A", "123456").await.unwrap();
            assert!(installed.view.signed_in && installed.view.auth_epoch == "3");
            assert!(installed.operation_id == OP2 && installed.originating_auth_epoch == "2");
            assert!(installed.reserved_auth_epoch.is_none());
            assert!(installed.view.cancel_target.as_ref().unwrap().operation_id == OP1);
            io.raw(Route::Cancel, 200, b"");
            io.raw(Route::Cancel, 200, b"");
            io.raw_logout();
            let cancelled = auth.cancel_login("1", OP3, OP1).await.unwrap();
            assert!(!cancelled.view.signed_in && cancelled.error.is_some());
            assert!(cancelled.view.auth_epoch == TERMINAL.to_string());
            assert!(auth.flow.lock().await.unknown_issuance);
        });
    }
    #[test]
    fn cancellation_reserved_during_exchange_refuses_session_store_publication() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            auth.view().await.unwrap();
            let state = "b".repeat(64);
            io.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
                "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") }));
            let begun = auth.begin_login("1", OP1).await.unwrap();
            auth.open_begin(begun, |_| Ok(())).await;
            let response = io.defer(Route::Exchange);
            let task_auth = auth.clone();
            let exchange = tauri::async_runtime::spawn(async move {
                task_auth.exchange_callback(&format!("citadel://auth/callback?code={}&state={}",
                    "a".repeat(64), "b".repeat(64))).await
            });
            io.await_count(2).await;
            // This is the actual pre-lock reservation used by cancel_login;
            // exchange still owns the async flow lock awaiting its response.
            let reserved = auth.reserve_control(ControlKind::Cancel, "1", OP2, Some(OP1)).unwrap();
            io.raw_logout();
            assert!(response.send(RawResponse::Scripted { status: StatusCode::OK,
                body: serde_json::to_vec(&tokens("B")).unwrap() }).is_ok());
            let outcome = exchange.await.unwrap().unwrap();
            assert!(outcome.superseded);
            assert!(io.writes.load(Ordering::SeqCst) == 0);
            let closed = auth.close_reserved(reserved).await;
            assert!(!closed.view.signed_in && closed.error.is_none());
            assert!(io.request_count() == 3);
        });
    }
    #[test]
    fn uncertain_refresh_write_is_disabled_and_cleanup_matches_the_written_owned_entry() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            io.fail_after_write.store(true, Ordering::SeqCst);
            io.raw(Route::Nodes, 401, b"");
            io.json(Route::Refresh, 200, tokens("rotated-A"));
            io.raw_logout();
            assert!(auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.is_err());
            assert!(auth.view().await.unwrap().auth_epoch == TERMINAL.to_string());
            io.raw_logout();
            io.raw_logout();
            let cleaned = auth.sign_out(&TERMINAL.to_string(), OP1).await.unwrap();
            assert!(cleaned.error.is_none());
            assert!(io.stored.lock().unwrap().is_none());
            assert!(io.deletes.load(Ordering::SeqCst) == 1);
            assert!(auth.capture_origin("1").await.is_err());
        });
    }
    #[test]
    fn counter_exhaustion_latches_terminal_and_still_allows_exact_cleanup() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            {
                let mut gate = auth.gate();
                gate.revision = TERMINAL - 1;
                gate.view.view_revision = gate.revision.to_string();
            }
            io.raw_logout();
            let first = auth.sign_out("1", OP1).await.unwrap();
            assert!(first.originating_auth_epoch == "1" && first.reserved_auth_epoch.is_none());
            assert!(first.view.auth_epoch == TERMINAL.to_string() && !first.superseded);
            assert!(auth.view().await.unwrap().auth_epoch == TERMINAL.to_string());
            let cleanup = auth.sign_out(&TERMINAL.to_string(), OP2).await.unwrap();
            assert!(cleanup.view.auth_epoch == TERMINAL.to_string() && cleanup.error.is_none());
            assert!(io.deletes.load(Ordering::SeqCst) == 1);
            assert!(auth.capture_origin("1").await.is_err());
        });
    }
    async fn start(auth: &AuthState, io: &TestIo, expose: bool) -> Option<BegunLogin> {
        let state = "b".repeat(64);
        io.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
            "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") }));
        let begun = auth.begin_login("1", OP1).await.unwrap();
        if expose { auth.open_begin(begun, |_| Ok(())).await; None } else { Some(begun) }
    }
    fn callback() -> String {
        format!("citadel://auth/callback?code={}&state={}", "a".repeat(64), "b".repeat(64))
    }
    #[test]
    fn browser_exposure_without_callback_blocks_new_signins_after_cancel_ack() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            auth.view().await.unwrap();
            start(&auth, &io, true).await;
            io.raw(Route::Cancel, 200, b"");
            let cancelled = auth.cancel_login("1", OP2, OP1).await.unwrap();
            assert!(cancelled.view.auth_epoch == TERMINAL.to_string() && cancelled.error.is_some());
            assert!(cancelled.reserved_auth_epoch.as_deref() == Some("3") && !cancelled.superseded);
            assert!(auth.begin_login("3", OP3).await.is_err());
            let retried = auth.sign_out(&TERMINAL.to_string(), OP3).await.unwrap();
            assert!(retried.error.is_some() && io.request_count() == 2);
            assert!(auth.flow.lock().await.cleanup.as_ref().unwrap().state.as_ref().unwrap().acknowledged);
        });
    }
    #[test]
    fn never_exposed_start_can_cancel_but_old_begin_cannot_open_later() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            auth.view().await.unwrap();
            let begun = start(&auth, &io, false).await.unwrap();
            io.raw(Route::Cancel, 200, b"");
            let cancelled = auth.cancel_login("1", OP2, OP1).await.unwrap();
            assert!(cancelled.error.is_none() && cancelled.view.auth_epoch == "3");
            let opened = std::sync::atomic::AtomicBool::new(false);
            let old = auth.open_begin(begun, |_| { opened.store(true, Ordering::SeqCst); Ok(()) }).await;
            assert!(old.superseded && !opened.load(Ordering::SeqCst));
            assert!(!auth.flow.lock().await.unknown_issuance);
        });
    }
    #[test]
    fn failed_browser_handoff_is_not_proof_of_no_issuance() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            auth.view().await.unwrap();
            let begun = start(&auth, &io, false).await.unwrap();
            let original_owner = begun.owner.owner.clone();
            let captured = begun.owner.initial_view.clone();
            io.raw(Route::Cancel, 200, b"");
            let failed = auth.open_begin(begun, |_| Err(())).await;
            assert!(failed.error.is_some() && failed.superseded);
            assert!(failed.operation_id == original_owner.operation_id && original_owner.kind == ControlKind::Begin);
            assert!(failed.originating_auth_epoch == original_owner.caller_epoch.to_string());
            assert!(failed.reserved_auth_epoch.as_deref() == Some("2"));
            assert!(failed.view.auth_epoch == captured.auth_epoch && failed.view.view_revision == captured.view_revision);
            assert!(!failed.view.signed_in && failed.view.factors.is_empty());
            {
                let gate = auth.gate();
                let cleanup_owner = gate.terminal_owner.as_ref().unwrap();
                assert!(cleanup_owner.kind == ControlKind::Cancel && cleanup_owner.reserved_epoch == 3);
                assert!(cleanup_owner.operation_id == original_owner.operation_id && cleanup_owner.caller_epoch == 1);
            }
            let emitted = std::sync::atomic::AtomicBool::new(false);
            assert!(!auth.publish_auth(&failed, |_| { emitted.store(true, Ordering::SeqCst); }));
            assert!(!emitted.load(Ordering::SeqCst));
            assert!(auth.view().await.unwrap().auth_epoch == TERMINAL.to_string());
            assert!(auth.flow.lock().await.unknown_issuance);
        });
    }
    #[test]
    fn valid_returned_state_survives_bad_sibling_but_uppercase_does_not_gain_authority() {
        tauri::async_runtime::block_on(async {
            for uppercase in [false, true] {
                let (auth, io) = AuthState::fixture(None);
                auth.view().await.unwrap();
                let state = if uppercase { "B".repeat(64) } else { "b".repeat(64) };
                io.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
                    "login_url": "https://invalid.example/" }));
                if !uppercase { io.raw(Route::Cancel, 200, b""); }
                let result = auth.begin_login("1", OP1).await.unwrap();
                assert!(result.url.is_none() && result.outcome.error.is_some());
                let sent = io.sent.lock().unwrap();
                assert!(sent.len() == if uppercase { 1 } else { 2 });
                if !uppercase { assert!(sent[1].body == json!({ "state": "b".repeat(64) })); }
                drop(sent);
                assert!(auth.flow.lock().await.unknown_issuance == uppercase);
            }
        });
    }
    #[test]
    fn pending202_already_has_hidden_credentials_without_mfa_send() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            auth.view().await.unwrap();
            start(&auth, &io, true).await;
            io.json(Route::Exchange, 202, json!({ "mfa_required": true,
                "pending_id": "d".repeat(64), "factors": [{ "id": "factor-A" }] }));
            let pending = auth.exchange_callback(&callback()).await.unwrap();
            assert!(pending.view.mfa_pending);
            io.raw(Route::Cancel, 503, b"");
            io.raw(Route::Cancel, 200, b"");
            let closed = auth.cancel_login("1", OP2, OP1).await.unwrap();
            assert!(closed.error.is_some() && closed.view.auth_epoch == TERMINAL.to_string());
            io.raw(Route::Cancel, 200, b"");
            let retried = auth.sign_out(&TERMINAL.to_string(), OP3).await.unwrap();
            assert!(retried.error.is_some());
            let flow = auth.flow.lock().await;
            let chain = flow.cleanup.as_ref().unwrap();
            assert!(chain.hidden_issued && !chain.mfa_admitted && flow.unknown_issuance);
            assert!(chain.pending.as_ref().unwrap().acknowledged);
            assert!(io.sent.lock().unwrap().iter().all(|sent| !matches!(sent.route, Route::Mfa)));
        });
    }
    #[test]
    fn valid_pending_identity_survives_bad_factors_but_false_discriminator_is_not_authority() {
        tauri::async_runtime::block_on(async {
            for valid_discriminator in [true, false] {
                let (auth, io) = AuthState::fixture(None);
                auth.view().await.unwrap();
                start(&auth, &io, true).await;
                io.json(Route::Exchange, 202, json!({ "mfa_required": valid_discriminator,
                    "pending_id": "d".repeat(64), "factors": [] }));
                if valid_discriminator { io.raw(Route::Cancel, 200, b""); }
                io.raw(Route::Cancel, 200, b"");
                let result = auth.exchange_callback(&callback()).await.unwrap();
                assert!(result.error.is_some() && result.view.auth_epoch == TERMINAL.to_string());
                let flow = auth.flow.lock().await;
                assert!(flow.cleanup.as_ref().unwrap().pending.is_some() == valid_discriminator);
                assert!(flow.unknown_issuance);
            }
        });
    }
    #[test]
    fn consumed_exchange_handle_survives_lost_response_and_later_cancel_ack() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None);
            auth.view().await.unwrap();
            start(&auth, &io, true).await;
            let lost = io.defer(Route::Exchange);
            drop(lost);
            io.raw(Route::Cancel, 503, b"");
            let failed = auth.exchange_callback(&callback()).await.unwrap();
            assert!(failed.view.auth_epoch == TERMINAL.to_string());
            {
                let flow = auth.flow.lock().await;
                assert!(flow.attempt.is_none());
                let chain = flow.cleanup.as_ref().unwrap();
                assert!(chain.exchange_admitted && chain.state.as_ref().unwrap().value == "b".repeat(64));
                assert!(!chain.state.as_ref().unwrap().acknowledged);
            }
            io.raw(Route::Cancel, 200, b"");
            let cleaned = auth.sign_out(&TERMINAL.to_string(), OP2).await.unwrap();
            assert!(cleaned.error.is_some() && auth.flow.lock().await.unknown_issuance);
        });
    }
    #[test]
    fn departed_token_survives_local_delete_failed_revoke_and_exact_retry() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            io.raw(Route::Logout, 503, b"");
            io.raw(Route::Logout, 503, b"");
            let failed = auth.sign_out("1", OP1).await.unwrap();
            assert!(failed.view.auth_epoch == TERMINAL.to_string() && failed.error.is_some());
            assert!(io.stored.lock().unwrap().is_none());
            assert!(auth.flow.lock().await.session.as_ref().unwrap().value.access_token == "fixture-access-A");
            io.raw_logout();
            let cleaned = auth.sign_out(&TERMINAL.to_string(), OP2).await.unwrap();
            assert!(cleaned.error.is_none());
            assert!(auth.flow.lock().await.session.is_none());
            assert!(io.sent.lock().unwrap().iter().all(|s| s.token.as_deref() == Some("fixture-access-A")));
        });
    }
    #[test]
    fn cleanup_refresh_keeps_both_old_and_new_exact_tokens_after_failure() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            {
                let mut flow = auth.flow.lock().await;
                flow.session.as_mut().unwrap().value.expires_at = 1;
                *io.stored.lock().unwrap() = Some(flow.session.as_ref().unwrap().value.clone());
            }
            io.json(Route::Refresh, 200, tokens("new-A"));
            for _ in 0..4 { io.raw(Route::Logout, 503, b""); }
            let failed = auth.sign_out("1", OP1).await.unwrap();
            assert!(failed.error.is_some() && failed.reserved_auth_epoch.as_deref() == Some("2"));
            {
                let flow = auth.flow.lock().await;
                assert!(flow.session.as_ref().unwrap().value.access_token == "fixture-access-A");
                assert!(flow.uncertain_session.as_ref().unwrap().access_token == "fixture-access-new-A");
                assert!(!flow.unknown_issuance);
            }
            io.raw_logout(); io.raw_logout();
            assert!(auth.sign_out(&TERMINAL.to_string(), OP2).await.unwrap().error.is_none());
            let sent = io.sent.lock().unwrap();
            assert!(sent.iter().filter(|s| matches!(s.route, Route::Refresh)).count() == 1);
            assert!(sent[5].token.as_deref() == Some("fixture-access-A"));
            assert!(sent[6].token.as_deref() == Some("fixture-access-new-A"));
        });
    }
    #[test]
    fn lost_cleanup_refresh_issuance_is_not_resolved_by_old_token_revocation() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            {
                let mut flow = auth.flow.lock().await;
                flow.session.as_mut().unwrap().value.expires_at = 1;
                *io.stored.lock().unwrap() = Some(flow.session.as_ref().unwrap().value.clone());
            }
            drop(io.defer(Route::Refresh));
            io.raw_logout();
            let failed = auth.sign_out("1", OP1).await.unwrap();
            assert!(failed.error.is_some());
            let retried = auth.sign_out(&TERMINAL.to_string(), OP2).await.unwrap();
            assert!(retried.error.is_some() && io.request_count() == 2);
            let flow = auth.flow.lock().await;
            assert!(flow.unknown_issuance && flow.unknown_refresh_origin.as_ref().unwrap().epoch == 1);
        });
    }
    #[test]
    fn delayed_body_read_and_mutating_results_cannot_publish_to_replacement() {
        tauri::async_runtime::block_on(async {
            for mutate in [false, true] {
                for body_failure in [false, true] {
                    let (auth, io) = AuthState::signed_in_fixture();
                    auth.view().await.unwrap();
                    let origin = auth.capture_origin("1").await.unwrap();
                    let route = if mutate { Route::Approve } else { Route::Nodes };
                    let (entered, body) = io.body(route, 200);
                    let task_auth = auth.clone();
                    let request = tauri::async_runtime::spawn(async move {
                        task_auth.request_bound(&origin, route.method(), route.path(),
                            mutate.then(|| json!({ "user_code": "ABCD-1234" }))).await
                    });
                    entered.await.unwrap(); // Real read_response is now inside json's body await.
                    io.raw_logout();
                    let departed = auth.sign_out("1", OP1).await.unwrap();
                    let replacement = login(&auth, &io, &departed.view.auth_epoch, OP2).await;
                    assert!(replacement.view.signed_in);
                    let bytes = if mutate { b"{}".to_vec() } else { b"[]".to_vec() };
                    assert!(body.send(if body_failure { Err("Fixture lost body".into()) } else { Ok(bytes) }).is_ok());
                    assert!(request.await.unwrap().is_err());
                    assert!(auth.view().await.unwrap().auth_epoch == replacement.view.auth_epoch);
                    let sent = io.sent.lock().unwrap();
                    assert!(sent[0].method == route.method() && sent[0].token.as_deref() == Some("fixture-access-A"));
                    if mutate { assert!(sent[0].body == json!({ "user_code": "ABCD-1234" })); }
                }
            }
        });
    }
    #[test]
    fn concurrent401_reuses_same_lineage_rotation_without_obsolete_refresh() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            let first = io.defer(Route::Nodes);
            let second = io.defer(Route::Nodes);
            io.json(Route::Refresh, 200, tokens("rotated-A"));
            io.json(Route::Nodes, 200, json!([]));
            io.json(Route::Nodes, 200, json!([]));
            let a = auth.clone(); let o = origin.clone();
            let one = tauri::async_runtime::spawn(async move {
                a.request_bound(&o, Method::GET, "/api/fabric/nodes", None).await
            });
            let a = auth.clone();
            let two = tauri::async_runtime::spawn(async move {
                a.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await
            });
            io.await_count(2).await;
            assert!(first.send(RawResponse::Scripted { status: StatusCode::UNAUTHORIZED, body: vec![] }).is_ok());
            io.await_count(4).await; // First refresh and retry have been admitted.
            assert!(second.send(RawResponse::Scripted { status: StatusCode::UNAUTHORIZED, body: vec![] }).is_ok());
            assert!(one.await.unwrap().is_ok() && two.await.unwrap().is_ok());
            let sent = io.sent.lock().unwrap();
            assert!(sent.len() == 5 && sent.iter().filter(|s| matches!(s.route, Route::Refresh)).count() == 1);
            assert!(sent[2].method == Method::POST && sent[2].body ==
                json!({ "refresh_token": "fixture-refresh-A", "device_id": "fixture-device-A" }));
            assert!(sent[3..].iter().all(|s| s.token.as_deref() == Some("fixture-access-rotated-A")));
        });
    }
    #[test]
    fn original_and_retry_handoffs_refuse_invalidation_without_sending() {
        tauri::async_runtime::block_on(async {
            for retry in [false, true] {
                let (auth, io) = AuthState::signed_in_fixture();
                auth.view().await.unwrap();
                let origin = auth.capture_origin("1").await.unwrap();
                if retry {
                    io.raw(Route::Nodes, 401, b"");
                    io.json(Route::Refresh, 200, tokens("rotated-A"));
                }
                let (entered, release) = io.hold(if retry { TestPoint::RetryAdmission } else { TestPoint::OriginalAdmission });
                let a = auth.clone();
                let request = tauri::async_runtime::spawn(async move {
                    a.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await
                });
                entered.await.unwrap();
                io.raw_logout();
                let closed = auth.sign_out("1", OP1).await.unwrap();
                assert!(closed.error.is_none());
                assert!(release.send(()).is_ok());
                assert!(request.await.unwrap().is_err());
                let sent = io.sent.lock().unwrap();
                assert!(sent.len() == if retry { 3 } else { 1 });
                assert!(matches!(sent.last().unwrap().route, Route::Logout));
                assert!(sent.last().unwrap().token.as_deref() == Some(if retry {
                    "fixture-access-rotated-A" } else { "fixture-access-A" }));
                drop(sent);
                assert!(!auth.flow.lock().await.unknown_issuance);
            }
        });
    }
    #[test]
    fn refresh_preadmission_refusal_is_not_unknown_issuance() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            io.raw(Route::Nodes, 401, b"");
            let (entered, release) = io.hold(TestPoint::RefreshAdmission);
            let a = auth.clone();
            let request = tauri::async_runtime::spawn(async move {
                a.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await
            });
            entered.await.unwrap();
            let reservation = auth.reserve_control(ControlKind::Logout, "1", OP1, None).unwrap();
            assert!(release.send(()).is_ok());
            assert!(request.await.unwrap().is_err());
            assert!(!auth.flow.lock().await.unknown_issuance);
            io.raw_logout();
            assert!(auth.close_reserved(reservation).await.error.is_none());
            assert!(io.sent.lock().unwrap().iter().all(|s| !matches!(s.route, Route::Refresh)));
        });
    }
    #[test]
    fn admitted_refresh_body_failure_retains_old_origin_and_never_repairs_as_new_owner() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            io.raw(Route::Nodes, 401, b"");
            let (entered, body) = io.body(Route::Refresh, 200);
            let a = auth.clone();
            let request = tauri::async_runtime::spawn(async move {
                a.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await
            });
            entered.await.unwrap();
            let reservation = auth.reserve_control(ControlKind::Logout, "1", OP1, None).unwrap();
            assert!(body.send(Ok(b"not-json".to_vec())).is_ok());
            assert!(request.await.unwrap().is_err());
            {
                let flow = auth.flow.lock().await;
                assert!(flow.unknown_issuance && flow.unknown_refresh_origin.as_ref().unwrap().epoch == 1);
                assert!(flow.session.as_ref().unwrap().value.access_token == "fixture-access-A");
            }
            io.raw_logout();
            let closed = auth.close_reserved(reservation).await;
            assert!(closed.originating_auth_epoch == "1" && closed.reserved_auth_epoch.as_deref() == Some("2"));
            assert!(!closed.superseded && closed.error.is_some() && closed.view.auth_epoch == TERMINAL.to_string());
            assert!(io.request_count() == 3);
        });
    }
    #[test]
    fn stale_refresh_success_and401_cannot_commit_or_clear_after_invalidation() {
        tauri::async_runtime::block_on(async {
            for success in [true, false] {
                let (auth, io) = AuthState::signed_in_fixture();
                auth.view().await.unwrap();
                let origin = auth.capture_origin("1").await.unwrap();
                io.raw(Route::Nodes, 401, b"");
                let refresh = io.defer(Route::Refresh);
                let a = auth.clone();
                let request = tauri::async_runtime::spawn(async move {
                    a.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await
                });
                io.await_count(2).await;
                let reservation = auth.reserve_control(ControlKind::Logout, "1", OP1, None).unwrap();
                if success { io.raw_logout(); }
                assert!(refresh.send(RawResponse::Scripted {
                    status: if success { StatusCode::OK } else { StatusCode::UNAUTHORIZED },
                    body: if success { serde_json::to_vec(&tokens("late-A")).unwrap() } else { b"not-json".to_vec() },
                }).is_ok());
                assert!(request.await.unwrap().is_err());
                assert!(io.writes.load(Ordering::SeqCst) == 0 && io.deletes.load(Ordering::SeqCst) == 0);
                io.raw_logout();
                let closed = auth.close_reserved(reservation).await;
                assert!(closed.error.is_none());
                let replacement = login(&auth, &io, &closed.view.auth_epoch, OP2).await;
                assert!(io.stored.lock().unwrap().as_ref().unwrap().access_token == "fixture-access-B");
                assert!(auth.view().await.unwrap().auth_epoch == replacement.view.auth_epoch);
            }
        });
    }
    #[test]
    fn deletion_failure_before_or_after_effect_retains_exact_cleanup_progress() {
        tauri::async_runtime::block_on(async {
            for after in [false, true] {
                for refresh401 in [false, true] {
                    let (auth, io) = AuthState::signed_in_fixture();
                    auth.view().await.unwrap();
                    io.fail_delete.store(!after, Ordering::SeqCst);
                    io.fail_after_delete.store(after, Ordering::SeqCst);
                    if refresh401 {
                        let origin = auth.capture_origin("1").await.unwrap();
                        io.raw(Route::Nodes, 401, b""); io.raw(Route::Refresh, 401, b"not-json");
                        assert!(auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.is_err());
                    } else {
                        io.raw_logout();
                        let failed = auth.sign_out("1", OP1).await.unwrap();
                        assert!(failed.error.is_some() && failed.view.auth_epoch == TERMINAL.to_string());
                    }
                    assert!(auth.flow.lock().await.session.is_some());
                    io.fail_delete.store(false, Ordering::SeqCst);
                    io.fail_after_delete.store(false, Ordering::SeqCst);
                    if refresh401 { io.raw_logout(); }
                    assert!(auth.sign_out(&TERMINAL.to_string(), OP2).await.unwrap().error.is_none());
                    assert!(io.stored.lock().unwrap().is_none() && auth.flow.lock().await.session.is_none());
                }
            }
        });
    }
    #[test]
    fn login_and_mfa_store_failures_keep_validated_candidate_and_hidden_uncertainty() {
        tauri::async_runtime::block_on(async {
            for mfa in [false, true] {
                for after in [false, true] {
                    let (auth, io) = AuthState::fixture(None);
                    auth.view().await.unwrap(); start(&auth, &io, true).await;
                    if mfa {
                        io.json(Route::Exchange, 202, json!({ "mfa_required": true,
                            "pending_id": "d".repeat(64), "factors": [{ "id": "factor-A" }] }));
                        assert!(auth.exchange_callback(&callback()).await.unwrap().view.mfa_pending);
                    }
                    io.fail_write.store(!after, Ordering::SeqCst);
                    io.fail_after_write.store(after, Ordering::SeqCst);
                    io.json(if mfa { Route::Mfa } else { Route::Exchange }, 200, tokens("candidate"));
                    io.raw_logout();
                    let failed = if mfa { auth.verify_mfa("2", OP2, "factor-A", "123456").await.unwrap() }
                        else { auth.exchange_callback(&callback()).await.unwrap() };
                    assert!(failed.view.auth_epoch == TERMINAL.to_string());
                    {
                        let flow = auth.flow.lock().await;
                        assert!(flow.uncertain_session.as_ref().unwrap().access_token == "fixture-access-candidate");
                        assert!(flow.candidate_revoked && flow.session.is_none());
                    }
                    if mfa { io.raw(Route::Cancel, 200, b""); io.raw(Route::Cancel, 200, b""); }
                    let cleaned = auth.sign_out(&TERMINAL.to_string(), OP3).await.unwrap();
                    assert!(cleaned.error.is_some() == mfa);
                    assert!(io.stored.lock().unwrap().is_none());
                }
            }
        });
    }
    #[test]
    fn terminal_logout_ticket_is_not_available_to_stale_foreign_or_pre_disabled_calls() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            auth.view().await.unwrap();
            {
                let mut gate = auth.gate(); gate.revision = TERMINAL - 1;
                gate.view.view_revision = gate.revision.to_string();
            }
            assert!(auth.sign_out("2", OP1).await.is_err());
            assert!(auth.sign_out("1", "invalid").await.is_err());
            assert!(auth.gate().phase == Phase::Session && io.request_count() == 0);
            let ticket = auth.reserve_control(ControlKind::Logout, "1", OP1, None).unwrap();
            assert!(ticket.overflow_cleanup);
            assert!(auth.sign_out("1", OP2).await.is_err());
            let mut foreign = ticket.clone(); foreign.owner.operation_id = OP2.into();
            assert!(auth.close_reserved(foreign).await.superseded && io.request_count() == 0);
            io.raw_logout();
            let exact = auth.close_reserved(ticket).await;
            assert!(exact.originating_auth_epoch == "1" && exact.reserved_auth_epoch.is_none());
            assert!(!exact.superseded && exact.view.auth_epoch == TERMINAL.to_string());
            assert!(io.request_count() == 1);
        });
    }
    #[test]
    fn finalization_overflow_keeps_successfully_reserved_epoch_and_owned_target() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture(); auth.view().await.unwrap();
            {
                let mut gate = auth.gate(); gate.revision = TERMINAL - 2;
                gate.view.view_revision = gate.revision.to_string();
            }
            io.raw_logout();
            let closed = auth.sign_out("1", OP1).await.unwrap();
            assert!(closed.originating_auth_epoch == "1" && closed.reserved_auth_epoch.as_deref() == Some("2"));
            assert!(!closed.superseded && closed.view.auth_epoch == TERMINAL.to_string());
            assert!(io.deletes.load(Ordering::SeqCst) == 1);
        });
    }
    #[test]
    fn login_commit_invalidated_first_never_reaches_actual_store_write() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap();
            start(&auth, &io, true).await;
            io.json(Route::Exchange, 200, tokens("B")); io.raw_logout();
            let (entered, release) = io.hold(TestPoint::LoginCommit);
            let a = auth.clone();
            let exchange = tauri::async_runtime::spawn(async move { a.exchange_callback(&callback()).await });
            entered.await.unwrap();
            let reserved = auth.reserve_control(ControlKind::Cancel, "1", OP2, Some(OP1)).unwrap();
            assert!(release.send(()).is_ok());
            assert!(exchange.await.unwrap().unwrap().superseded);
            assert!(io.writes.load(Ordering::SeqCst) == 0);
            assert!(auth.close_reserved(reserved).await.error.is_none());
            assert!(io.stored.lock().unwrap().is_none());
        });
    }
    #[test]
    fn refresh_commit_invalidated_first_never_reaches_actual_store_write() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture(); auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            io.raw(Route::Nodes, 401, b""); io.json(Route::Refresh, 200, tokens("late-A"));
            io.raw_logout();
            let (entered, release) = io.hold(TestPoint::RefreshCommit);
            let a = auth.clone();
            let request = tauri::async_runtime::spawn(async move {
                a.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await
            });
            entered.await.unwrap();
            let reserved = auth.reserve_control(ControlKind::Logout, "1", OP1, None).unwrap();
            assert!(release.send(()).is_ok());
            assert!(request.await.unwrap().is_err());
            assert!(io.writes.load(Ordering::SeqCst) == 0);
            io.raw_logout(); assert!(auth.close_reserved(reserved).await.error.is_none());
        });
    }
    #[test]
    fn actual_store_write_holds_gate_until_install_then_invalidation_can_reserve() {
        let (auth, io) = AuthState::fixture(None);
        tauri::async_runtime::block_on(async {
            auth.view().await.unwrap(); start(&auth, &io, true).await;
        });
        io.json(Route::Exchange, 200, tokens("B"));
        let (entered, release) = io.hold_store(StorePoint::Write);
        let a = auth.clone();
        let writer = std::thread::spawn(move || tauri::async_runtime::block_on(a.exchange_callback(&callback())));
        entered.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(auth.authority.try_lock().is_err());
        let a = auth.clone();
        let (started, observed) = std::sync::mpsc::channel();
        let invalidator = std::thread::spawn(move || {
            started.send(()).unwrap();
            a.reserve_control(ControlKind::Logout, "3", OP2, None)
        });
        observed.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(auth.authority.try_lock().is_err());
        release.send(()).unwrap();
        let installed = writer.join().unwrap().unwrap();
        assert!(installed.view.signed_in && installed.view.auth_epoch == "3");
        let reserved = invalidator.join().unwrap().unwrap();
        assert!(reserved.owner.reserved_epoch == 4 && io.writes.load(Ordering::SeqCst) == 1);
        io.raw_logout();
        tauri::async_runtime::block_on(async { assert!(auth.close_reserved(reserved).await.error.is_none()); });
    }
    #[test]
    fn actual_store_delete_is_synchronous_under_authority_gate() {
        let (auth, io) = AuthState::signed_in_fixture();
        tauri::async_runtime::block_on(auth.view()).unwrap();
        io.raw_logout();
        let (entered, release) = io.hold_store(StorePoint::Delete);
        let a = auth.clone();
        let logout = std::thread::spawn(move || tauri::async_runtime::block_on(a.sign_out("1", OP1)));
        entered.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(auth.authority.try_lock().is_err());
        assert!(io.stored.lock().unwrap().is_some());
        release.send(()).unwrap();
        let closed = logout.join().unwrap().unwrap();
        assert!(closed.error.is_none() && io.stored.lock().unwrap().is_none());
        assert!(io.deletes.load(Ordering::SeqCst) == 1);
    }
    #[test]
    fn start_cancel_before_response_retains_only_returned_valid_owned_state() {
        tauri::async_runtime::block_on(async {
            for malformed in [false, true] {
                let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap();
                let response = io.defer(Route::Start);
                let a = auth.clone();
                let begin = tauri::async_runtime::spawn(async move { a.begin_login("1", OP1).await });
                io.await_count(1).await;
                let reserved = auth.reserve_control(ControlKind::Cancel, "1", OP2, Some(OP1)).unwrap();
                if !malformed { io.raw(Route::Cancel, 200, b""); }
                let state = "b".repeat(64);
                let body = if malformed { b"not-json".to_vec() } else {
                    serde_json::to_vec(&json!({ "state": state, "expires_in": 120,
                        "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") })).unwrap()
                };
                assert!(response.send(RawResponse::Scripted { status: StatusCode::OK, body }).is_ok());
                let old = begin.await.unwrap().unwrap();
                assert!(old.url.is_none() && old.outcome.superseded);
                let closed = auth.close_reserved(reserved).await;
                assert!(closed.error.is_some() == malformed);
                assert!((closed.view.auth_epoch == TERMINAL.to_string()) == malformed);
                assert!(io.request_count() == if malformed { 1 } else { 2 });
            }
        });
    }
    #[test]
    fn consumed_mfa_handle_survives_lost_body_and_cannot_be_cleared_by_ack() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap(); start(&auth, &io, true).await;
            io.json(Route::Exchange, 202, json!({ "mfa_required": true,
                "pending_id": "d".repeat(64), "factors": [{ "id": "factor-A" }] }));
            auth.exchange_callback(&callback()).await.unwrap();
            let (entered, body) = io.body(Route::Mfa, 200);
            let a = auth.clone();
            let mfa = tauri::async_runtime::spawn(async move { a.verify_mfa("2", OP2, "factor-A", "123456").await });
            entered.await.unwrap();
            io.raw(Route::Cancel, 200, b""); io.raw(Route::Cancel, 200, b"");
            assert!(body.send(Err("Fixture lost MFA body".into())).is_ok());
            let failed = mfa.await.unwrap().unwrap();
            assert!(failed.operation_id == OP2 && failed.originating_auth_epoch == "2" && failed.reserved_auth_epoch.is_none());
            assert!(failed.error.is_some() && failed.view.auth_epoch == TERMINAL.to_string());
            let flow = auth.flow.lock().await;
            assert!(flow.mfa.is_none() && flow.unknown_issuance);
            let chain = flow.cleanup.as_ref().unwrap();
            assert!(chain.mfa_admitted && chain.pending.as_ref().unwrap().value == "d".repeat(64));
        });
    }
    #[test]
    fn expired_exposed_attempt_and_pending_mfa_do_not_restore_new_login_authority() {
        tauri::async_runtime::block_on(async {
            for mfa in [false, true] {
                let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap(); start(&auth, &io, true).await;
                if mfa {
                    io.json(Route::Exchange, 202, json!({ "mfa_required": true,
                        "pending_id": "d".repeat(64), "factors": [{ "id": "factor-A" }] }));
                    auth.exchange_callback(&callback()).await.unwrap();
                }
                {
                    let mut flow = auth.flow.lock().await;
                    if mfa { flow.mfa.as_mut().unwrap().expires_at = Instant::now(); }
                    else { flow.attempt.as_mut().unwrap().expires_at = Instant::now(); }
                }
                if mfa { io.raw(Route::Cancel, 200, b""); }
                io.raw(Route::Cancel, 200, b"");
                let expired = auth.view().await.unwrap();
                assert!(expired.auth_epoch == TERMINAL.to_string() && expired.cancel_target.is_none());
                assert!(auth.begin_login("2", OP2).await.is_err());
            }
        });
    }
    #[test]
    fn expired_persisted_session_refreshes_its_own_lineage_and_exhausted_revision_refuses_send() {
        tauri::async_runtime::block_on(async {
            for exhausted in [false, true] {
                let (auth, io) = AuthState::signed_in_fixture();
                if exhausted {
                    auth.view().await.unwrap();
                    auth.flow.lock().await.session.as_mut().unwrap().revision = TERMINAL - 1;
                    let origin = auth.capture_origin("1").await.unwrap();
                    io.raw(Route::Nodes, 401, b"");
                    assert!(auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.is_err());
                    assert!(io.request_count() == 1 && !auth.flow.lock().await.unknown_issuance);
                } else {
                    io.stored.lock().unwrap().as_mut().unwrap().expires_at = 1;
                    io.json(Route::Refresh, 200, tokens("renewed-A"));
                    let view = auth.view().await.unwrap();
                    assert!(view.signed_in && view.auth_epoch == "1" && view.view_revision == "2");
                    assert!(io.request_count() == 1 && auth.flow.lock().await.session.as_ref().unwrap().revision == 1);
                }
            }
        });
    }
    #[test]
    fn exact_terminal_cleanup_does_not_delete_foreign_replacement_store() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture(); auth.view().await.unwrap();
            let mut foreign = io.stored.lock().unwrap().clone().unwrap(); foreign.access_token = "foreign-B".into();
            *io.stored.lock().unwrap() = Some(foreign.clone());
            assert!(auth.view().await.unwrap().auth_epoch == TERMINAL.to_string());
            io.raw_logout();
            let cleanup = auth.sign_out(&TERMINAL.to_string(), OP1).await.unwrap();
            assert!(cleanup.error.is_some() && io.deletes.load(Ordering::SeqCst) == 0);
            assert!(io.stored.lock().unwrap().as_ref() == Some(&foreign));
        });
    }
    #[test]
    fn mfa_cancel_during_actual_body_decode_cannot_install_or_erase_hidden_issuance() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap(); start(&auth, &io, true).await;
            io.json(Route::Exchange, 202, json!({ "mfa_required": true,
                "pending_id": "d".repeat(64), "factors": [{ "id": "factor-A" }] }));
            auth.exchange_callback(&callback()).await.unwrap();
            let (entered, body) = io.body(Route::Mfa, 200);
            let a = auth.clone();
            let mfa = tauri::async_runtime::spawn(async move { a.verify_mfa("2", OP2, "factor-A", "123456").await });
            entered.await.unwrap();
            let reserved = auth.reserve_control(ControlKind::Cancel, "1", OP3, Some(OP1)).unwrap();
            io.raw_logout();
            assert!(body.send(Ok(serde_json::to_vec(&tokens("late-MFA")).unwrap())).is_ok());
            let old = mfa.await.unwrap().unwrap();
            assert!(old.superseded && old.operation_id == OP2 && old.view.auth_epoch == "2");
            assert!(io.writes.load(Ordering::SeqCst) == 0);
            io.raw(Route::Cancel, 200, b""); io.raw(Route::Cancel, 200, b"");
            let closed = auth.close_reserved(reserved).await;
            assert!(closed.error.is_some() && !closed.superseded && closed.view.auth_epoch == TERMINAL.to_string());
            assert!(auth.flow.lock().await.unknown_issuance);
        });
    }
    #[test]
    fn two_known_slots_refuse_another_credential_producing_admission() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture(); auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            {
                let mut flow = auth.flow.lock().await;
                let payload = serde_json::from_value::<TokenResponse>(tokens("candidate-A")).unwrap();
                flow.uncertain_session = Some(Session::from_response(payload, None));
                flow.candidate_origin = Some(CandidateOrigin::Lineage(origin.lineage));
            }
            io.raw(Route::Nodes, 401, b"");
            assert!(auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.is_err());
            assert!(io.request_count() == 1 && auth.gate().phase == Phase::Disabled);
            {
                let flow = auth.flow.lock().await;
                assert!(flow.session.as_ref().unwrap().value.access_token == "fixture-access-A");
                assert!(flow.uncertain_session.as_ref().unwrap().access_token == "fixture-access-candidate-A");
            }
            io.raw_logout(); io.raw_logout();
            assert!(auth.sign_out(&TERMINAL.to_string(), OP1).await.unwrap().error.is_none());
            assert!(io.request_count() == 3);
        });
    }
    #[test]
    fn admitted_normal_refresh_malformed_or_server_failure_is_sticky_not_another_refresh() {
        tauri::async_runtime::block_on(async {
            for malformed in [true, false] {
                let (auth, io) = AuthState::signed_in_fixture(); auth.view().await.unwrap();
                let origin = auth.capture_origin("1").await.unwrap();
                io.raw(Route::Nodes, 401, b"");
                io.raw(Route::Refresh, if malformed { 200 } else { 503 }, b"not-json");
                assert!(auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.is_err());
                assert!(auth.view().await.unwrap().auth_epoch == TERMINAL.to_string());
                assert!(io.writes.load(Ordering::SeqCst) == 0 && io.deletes.load(Ordering::SeqCst) == 0);
                io.raw_logout();
                assert!(auth.sign_out(&TERMINAL.to_string(), OP1).await.unwrap().error.is_some());
                assert!(auth.flow.lock().await.unknown_refresh_origin.as_ref().unwrap().epoch == 1);
                assert!(io.request_count() == 3);
            }
        });
    }
    #[test]
    fn normal_refresh_write_failures_preserve_old_and_candidate_custody() {
        tauri::async_runtime::block_on(async {
            for after in [false, true] {
                let (auth, io) = AuthState::signed_in_fixture(); auth.view().await.unwrap();
                let origin = auth.capture_origin("1").await.unwrap();
                io.fail_write.store(!after, Ordering::SeqCst); io.fail_after_write.store(after, Ordering::SeqCst);
                io.raw(Route::Nodes, 401, b""); io.json(Route::Refresh, 200, tokens("candidate-A")); io.raw_logout();
                assert!(auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.is_err());
                {
                    let flow = auth.flow.lock().await;
                    assert!(flow.session.as_ref().unwrap().value.access_token == "fixture-access-A");
                    assert!(flow.uncertain_session.as_ref().unwrap().access_token == "fixture-access-candidate-A");
                    assert!(flow.candidate_revoked && !flow.unknown_issuance);
                }
                io.raw_logout();
                assert!(auth.sign_out(&TERMINAL.to_string(), OP1).await.unwrap().error.is_none());
                assert!(io.stored.lock().unwrap().is_none() && io.request_count() == 4);
            }
        });
    }
    #[test]
    fn store_read_failure_at_actual_login_commit_keeps_exact_candidate_not_foreign_authority() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap(); start(&auth, &io, true).await;
            io.json(Route::Exchange, 200, tokens("B")); io.raw_logout();
            let (entered, release) = io.hold(TestPoint::LoginCommit);
            let a = auth.clone();
            let exchange = tauri::async_runtime::spawn(async move { a.exchange_callback(&callback()).await });
            entered.await.unwrap(); io.fail_read.store(true, Ordering::SeqCst);
            assert!(release.send(()).is_ok());
            let failed = exchange.await.unwrap().unwrap();
            assert!(failed.view.auth_epoch == TERMINAL.to_string() && !failed.superseded);
            assert!(io.writes.load(Ordering::SeqCst) == 0);
            assert!(auth.flow.lock().await.uncertain_session.as_ref().unwrap().access_token == "fixture-access-B");
            io.fail_read.store(false, Ordering::SeqCst);
            assert!(auth.sign_out(&TERMINAL.to_string(), OP2).await.unwrap().error.is_none());
        });
    }
    #[test]
    fn epoch_exhaustion_uses_exact_logout_ticket_without_wrapping_or_new_admission() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture(); auth.view().await.unwrap();
            let expected = (TERMINAL - 1).to_string();
            {
                let mut gate = auth.gate(); gate.epoch = TERMINAL - 1;
                gate.view.auth_epoch = expected.clone();
            }
            io.raw_logout();
            let closed = auth.sign_out(&expected, OP1).await.unwrap();
            assert!(closed.originating_auth_epoch == expected && closed.reserved_auth_epoch.is_none());
            assert!(closed.view.auth_epoch == TERMINAL.to_string() && !closed.superseded);
            assert!(auth.capture_origin(&expected).await.is_err() && auth.begin_login(&expected, OP2).await.is_err());
            assert!(io.request_count() == 1);
        });
    }
    #[test]
    fn lost_start_body_without_any_handle_is_terminal_not_guessed_cleanup_authority() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap();
            let (entered, body) = io.body(Route::Start, 200);
            let a = auth.clone();
            let begin = tauri::async_runtime::spawn(async move { a.begin_login("1", OP1).await });
            entered.await.unwrap(); assert!(body.send(Err("Fixture lost start body".into())).is_ok());
            let failed = begin.await.unwrap().unwrap();
            assert!(failed.url.is_none() && failed.outcome.view.auth_epoch == TERMINAL.to_string());
            assert!(failed.outcome.originating_auth_epoch == "1"
                && failed.outcome.reserved_auth_epoch.as_deref() == Some("2") && !failed.outcome.superseded);
            let flow = auth.flow.lock().await;
            assert!(flow.cleanup.as_ref().unwrap().state.is_none() && flow.unknown_issuance);
            assert!(io.request_count() == 1);
        });
    }
    #[test]
    fn never_exposed_handle_retry_can_ack_known_cleanup_without_reviving_terminal_auth() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap();
            let _begun = start(&auth, &io, false).await.unwrap();
            // close_reserved runs clean_pending directly (to gate the departing
            // refresh) and again through clean_owned, so one failed close makes
            // two exact cancels of the same unacknowledged handle. Both fail.
            io.raw(Route::Cancel, 503, b""); io.raw(Route::Cancel, 503, b"");
            let failed = auth.cancel_login("1", OP2, OP1).await.unwrap();
            assert!(failed.error.is_some() && failed.view.auth_epoch == TERMINAL.to_string());
            assert!(io.request_count() == 3);
            {
                let flow = auth.flow.lock().await;
                assert!(!flow.cleanup.as_ref().unwrap().state.as_ref().unwrap().acknowledged);
            }
            io.raw(Route::Cancel, 200, b"");
            let cleaned = auth.sign_out(&TERMINAL.to_string(), OP3).await.unwrap();
            assert!(cleaned.error.is_none() && cleaned.view.auth_epoch == TERMINAL.to_string());
            assert!(!auth.flow.lock().await.unknown_issuance && auth.begin_login("3", OP1).await.is_err());
            assert!(io.request_count() == 4);
        });
    }
    #[test]
    fn proven401_removal_keeps_only_exact_token_cleanup_not_authenticated_authority() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture(); auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            io.raw(Route::Nodes, 401, b""); io.raw(Route::Refresh, 401, b"not-json");
            assert!(auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.is_err());
            let removed = auth.view().await.unwrap();
            assert!(!removed.signed_in && removed.auth_epoch == "2");
            assert!(auth.capture_origin("2").await.is_err());
            assert!(auth.flow.lock().await.session.as_ref().unwrap().value.access_token == "fixture-access-A");
            io.raw_logout();
            let cleaned = auth.sign_out("2", OP1).await.unwrap();
            assert!(cleaned.error.is_none() && cleaned.view.auth_epoch == "3");
            assert!(auth.flow.lock().await.session.is_none());
            assert!(io.sent.lock().unwrap().last().unwrap().token.as_deref() == Some("fixture-access-A"));
        });
    }
    #[test]
    fn mfa_empty_and_duplicate_factor_ids_never_reach_presentation_or_discard_pending_custody() {
        tauri::async_runtime::block_on(async {
            for factors in [json!([{ "id": "" }]), json!([{ "id": "same" }, { "id": "same" }])] {
                let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap(); start(&auth, &io, true).await;
                io.json(Route::Exchange, 202, json!({ "mfa_required": true,
                    "pending_id": "d".repeat(64), "factors": factors }));
                io.raw(Route::Cancel, 200, b""); io.raw(Route::Cancel, 200, b"");
                let rejected = auth.exchange_callback(&callback()).await.unwrap();
                assert!(rejected.error.is_some() && rejected.view.auth_epoch == TERMINAL.to_string());
                assert!(!rejected.view.mfa_pending && rejected.view.factors.is_empty());
                assert!(io.writes.load(Ordering::SeqCst) == 0);
                let flow = auth.flow.lock().await;
                let chain = flow.cleanup.as_ref().unwrap();
                assert!(chain.hidden_issued && flow.unknown_issuance);
                assert!(chain.pending.as_ref().unwrap().value == "d".repeat(64)
                    && chain.pending.as_ref().unwrap().acknowledged);
                assert!(io.sent.lock().unwrap().iter().all(|sent| !matches!(sent.route, Route::Mfa)));
            }
        });
    }
    #[test]
    fn delayed_original_approve401_cannot_refresh_retry_or_publish_as_replacement() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture(); auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            let response = io.defer(Route::Approve);
            let a = auth.clone();
            let request = tauri::async_runtime::spawn(async move {
                a.request_bound(&origin, Method::POST, "/api/fabric/device-auth/approve",
                    Some(json!({ "user_code": "ABCD-1234" }))).await
            });
            io.await_count(1).await; io.raw_logout();
            let departed = auth.sign_out("1", OP1).await.unwrap();
            let replacement = login(&auth, &io, &departed.view.auth_epoch, OP2).await;
            assert!(response.send(RawResponse::Scripted {
                status: StatusCode::UNAUTHORIZED, body: b"not-json".to_vec() }).is_ok());
            assert!(request.await.unwrap().is_err());
            assert!(auth.view().await.unwrap().auth_epoch == replacement.view.auth_epoch);
            assert!(io.stored.lock().unwrap().as_ref().unwrap().access_token == "fixture-access-B");
            let sent = io.sent.lock().unwrap();
            assert!(sent.len() == 4 && matches!(sent[0].route, Route::Approve));
            assert!(sent[0].method == Method::POST && sent[0].body == json!({ "user_code": "ABCD-1234" }));
            assert!(sent[0].token.as_deref() == Some("fixture-access-A"));
            assert!(sent.iter().filter(|s| matches!(s.route, Route::Approve)).count() == 1);
            assert!(sent.iter().all(|s| !matches!(s.route, Route::Refresh)));
        });
    }
    #[test]
    fn same_email_new_login_establishes_new_private_lineage_and_refuses_old_origin() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture();
            let initial = auth.view().await.unwrap();
            let old = auth.capture_origin("1").await.unwrap();
            io.raw_logout(); let departed = auth.sign_out("1", OP1).await.unwrap();
            let state = "b".repeat(64);
            io.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
                "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") }));
            let begun = auth.begin_login(&departed.view.auth_epoch, OP2).await.unwrap();
            auth.open_begin(begun, |_| Ok(())).await;
            let mut payload = tokens("replacement-A");
            payload["user"]["email"] = json!(initial.email.clone());
            io.json(Route::Exchange, 200, payload);
            let installed = auth.exchange_callback(&callback()).await.unwrap();
            assert!(installed.view.email == initial.email && installed.view.auth_epoch != initial.auth_epoch);
            let new = auth.capture_origin(&installed.view.auth_epoch).await.unwrap();
            assert!(new.lineage != old.lineage && new.epoch != old.epoch);
            let count = io.request_count();
            assert!(auth.request_bound(&old, Method::GET, "/api/fabric/nodes", None).await.is_err());
            assert!(io.request_count() == count);
            assert!(auth.flow.lock().await.session.as_ref().unwrap().value.access_token == "fixture-access-replacement-A");
        });
    }
    #[test]
    fn current_auth_outcome_is_published_but_the_same_outcome_is_refused_after_logout() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap();
            let installed = login(&auth, &io, "1", OP1).await;
            let delivered = std::sync::atomic::AtomicUsize::new(0);
            assert!(auth.publish_auth(&installed, |event| {
                assert!(event.operation_id == OP1 && event.view.signed_in);
                assert!(event.view.auth_epoch == installed.view.auth_epoch
                    && event.view.view_revision == installed.view.view_revision);
                delivered.fetch_add(1, Ordering::SeqCst);
            }));
            io.raw_logout(); let departed = auth.sign_out(&installed.view.auth_epoch, OP2).await.unwrap();
            assert!(!auth.publish_auth(&installed, |_| { delivered.fetch_add(1, Ordering::SeqCst); }));
            assert!(auth.publish_auth(&departed, |event| {
                assert!(!event.view.signed_in && event.operation_id == OP2);
                delivered.fetch_add(1, Ordering::SeqCst);
            }));
            assert!(delivered.load(Ordering::SeqCst) == 2);
        });
    }
    #[test]
    fn actual_mfa_write_holds_gate_until_install_before_queued_invalidation() {
        let (auth, io) = AuthState::fixture(None);
        tauri::async_runtime::block_on(async {
            auth.view().await.unwrap(); start(&auth, &io, true).await;
            io.json(Route::Exchange, 202, json!({ "mfa_required": true,
                "pending_id": "d".repeat(64), "factors": [{ "id": "factor-A" }] }));
            assert!(auth.exchange_callback(&callback()).await.unwrap().view.mfa_pending);
        });
        io.json(Route::Mfa, 200, tokens("MFA-B"));
        let (entered, release) = io.hold_store(StorePoint::Write);
        let a = auth.clone();
        let writer = std::thread::spawn(move || tauri::async_runtime::block_on(a.verify_mfa("2", OP2, "factor-A", "123456")));
        entered.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(auth.authority.try_lock().is_err());
        let a = auth.clone(); let (started, observed) = std::sync::mpsc::channel();
        let invalidator = std::thread::spawn(move || {
            started.send(()).unwrap(); a.reserve_control(ControlKind::Logout, "3", OP3, None)
        });
        observed.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(auth.authority.try_lock().is_err()); release.send(()).unwrap();
        let installed = writer.join().unwrap().unwrap();
        assert!(installed.view.signed_in && installed.view.auth_epoch == "3" && installed.operation_id == OP2);
        let reservation = invalidator.join().unwrap().unwrap();
        assert!(reservation.owner.reserved_epoch == 4 && io.writes.load(Ordering::SeqCst) == 1);
        io.raw(Route::Cancel, 200, b""); io.raw(Route::Cancel, 200, b""); io.raw_logout();
        tauri::async_runtime::block_on(async {
            let closed = auth.close_reserved(reservation).await;
            assert!(closed.error.is_some() && closed.view.auth_epoch == TERMINAL.to_string());
            assert!(auth.flow.lock().await.unknown_issuance);
        });
    }
    #[test]
    fn actual_normal_refresh_write_precedes_invalidation_and_retry_cannot_cross_it() {
        let (auth, io) = AuthState::signed_in_fixture();
        let origin = tauri::async_runtime::block_on(async {
            auth.view().await.unwrap(); auth.capture_origin("1").await.unwrap()
        });
        io.raw(Route::Nodes, 401, b""); io.json(Route::Refresh, 200, tokens("rotated-A"));
        let (entered, release) = io.hold_store(StorePoint::Write);
        let (retry_entered, retry_release) = io.hold(TestPoint::RetryAdmission);
        let a = auth.clone();
        let writer = std::thread::spawn(move || tauri::async_runtime::block_on(
            a.request_bound(&origin, Method::GET, "/api/fabric/nodes", None)));
        entered.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(auth.authority.try_lock().is_err());
        let a = auth.clone(); let (started, observed) = std::sync::mpsc::channel();
        let invalidator = std::thread::spawn(move || {
            started.send(()).unwrap(); a.reserve_control(ControlKind::Logout, "1", OP1, None)
        });
        observed.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(auth.authority.try_lock().is_err()); release.send(()).unwrap();
        let reservation = invalidator.join().unwrap().unwrap();
        tauri::async_runtime::block_on(async {
            tokio::time::timeout(Duration::from_secs(5), retry_entered).await.unwrap().unwrap();
        });
        assert!(retry_release.send(()).is_ok()); assert!(writer.join().unwrap().is_err());
        assert!(io.writes.load(Ordering::SeqCst) == 1 && io.request_count() == 2);
        assert!(io.stored.lock().unwrap().as_ref().unwrap().access_token == "fixture-access-rotated-A");
        io.raw_logout();
        tauri::async_runtime::block_on(async { assert!(auth.close_reserved(reservation).await.error.is_none()); });
        let sent = io.sent.lock().unwrap();
        assert!(sent.len() == 3 && sent[2].token.as_deref() == Some("fixture-access-rotated-A"));
    }
    #[test]
    fn actual_refresh401_delete_holds_gate_then_allows_only_exact_departed_cleanup() {
        let (auth, io) = AuthState::signed_in_fixture();
        let origin = tauri::async_runtime::block_on(async {
            auth.view().await.unwrap(); auth.capture_origin("1").await.unwrap()
        });
        io.raw(Route::Nodes, 401, b""); io.raw(Route::Refresh, 401, b"not-json");
        let (entered, release) = io.hold_store(StorePoint::Delete);
        let a = auth.clone();
        let rejection = std::thread::spawn(move || tauri::async_runtime::block_on(
            a.request_bound(&origin, Method::GET, "/api/fabric/nodes", None)));
        entered.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(auth.authority.try_lock().is_err() && io.stored.lock().unwrap().is_some());
        let a = auth.clone(); let (started, observed) = std::sync::mpsc::channel();
        let next = std::thread::spawn(move || {
            started.send(()).unwrap(); a.reserve_control(ControlKind::Logout, "2", OP1, None)
        });
        observed.recv_timeout(Duration::from_secs(5)).unwrap();
        assert!(auth.authority.try_lock().is_err()); release.send(()).unwrap();
        assert!(rejection.join().unwrap().is_err());
        let reservation = next.join().unwrap().unwrap();
        assert!(reservation.owner.reserved_epoch == 3 && reservation.departing_lineage == Some(1));
        assert!(io.stored.lock().unwrap().is_none() && io.deletes.load(Ordering::SeqCst) == 1);
        io.raw_logout();
        tauri::async_runtime::block_on(async {
            assert!(auth.capture_origin("2").await.is_err());
            assert!(auth.close_reserved(reservation).await.error.is_none());
            assert!(!auth.flow.lock().await.unknown_issuance);
        });
        assert!(io.request_count() == 3 && io.writes.load(Ordering::SeqCst) == 0);
    }
    #[test]
    fn pending_begin_supersession_cleans_exact_predecessor_before_new_start_and_refuses_old_work() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap();
            let old = start(&auth, &io, false).await.unwrap();
            let old_outcome = old.outcome.clone();
            let (entered, release) = io.hold(TestPoint::BeginReserved);
            let a = auth.clone();
            let replacement = tauri::async_runtime::spawn(async move { a.begin_login("2", OP2).await });
            entered.await.unwrap();
            let opened = std::sync::atomic::AtomicBool::new(false);
            assert!(auth.open_begin(old, |_| { opened.store(true, Ordering::SeqCst); Ok(()) }).await.superseded);
            assert!(!opened.load(Ordering::SeqCst));
            assert!(auth.exchange_callback(&callback()).await.is_err());
            {
                let flow = auth.flow.lock().await;
                let chain = flow.cleanup.as_ref().unwrap();
                assert!(chain.receipt.operation_id == OP1 && chain.state.as_ref().unwrap().value == "b".repeat(64));
                assert!(!chain.state.as_ref().unwrap().acknowledged);
            }
            io.raw(Route::Cancel, 200, b"");
            let state = "c".repeat(64);
            io.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
                "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") }));
            assert!(release.send(()).is_ok());
            let new = replacement.await.unwrap().unwrap();
            assert!(!new.outcome.superseded && new.outcome.operation_id == OP2 && new.outcome.view.auth_epoch == "3");
            assert!(auth.exchange_callback(&callback()).await.is_err());
            assert!(auth.cancel_login("1", OP3, OP1).await.is_err());
            assert!(!auth.publish_auth(&old_outcome, |_| panic!("Stale BEGIN event was handed off")));
            {
                let flow = auth.flow.lock().await;
                assert!(flow.attempt.as_ref().unwrap().state == state);
                assert!(flow.cleanup.as_ref().unwrap().receipt.operation_id == OP2);
            }
            auth.open_begin(new, |_| Ok(())).await;
            io.json(Route::Exchange, 200, tokens("new-B"));
            let installed = auth.exchange_callback(&format!(
                "citadel://auth/callback?code={}&state={state}", "a".repeat(64))).await.unwrap();
            assert!(installed.view.signed_in && installed.view.auth_epoch == "4");
            let sent = io.sent.lock().unwrap();
            assert!(sent.len() == 4 && matches!(sent[1].route, Route::Cancel));
            assert!(sent[1].body == json!({ "state": "b".repeat(64) }) && matches!(sent[2].route, Route::Start));
        });
    }
    #[test]
    fn newer_begin_waits_for_late_predecessor_start_cleanup_without_adopting_its_state() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap();
            let response = io.defer(Route::Start);
            let a = auth.clone();
            let old = tauri::async_runtime::spawn(async move { a.begin_login("1", OP1).await });
            io.await_count(1).await;
            let (entered, release) = io.hold(TestPoint::BeginReserved);
            let a = auth.clone();
            let new = tauri::async_runtime::spawn(async move { a.begin_login("2", OP2).await });
            entered.await.unwrap();
            io.raw(Route::Cancel, 200, b"");
            let old_state = "b".repeat(64);
            assert!(response.send(RawResponse::Scripted { status: StatusCode::OK,
                body: serde_json::to_vec(&json!({ "state": old_state, "expires_in": 120,
                    "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={old_state}") })).unwrap() }).is_ok());
            let old = old.await.unwrap().unwrap();
            assert!(old.url.is_none() && old.outcome.superseded && old.outcome.view.auth_epoch == "2");
            let state = "c".repeat(64);
            io.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
                "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") }));
            assert!(release.send(()).is_ok());
            let begun = new.await.unwrap().unwrap();
            assert!(begun.outcome.operation_id == OP2 && !begun.outcome.superseded && begun.url.is_some());
            assert!(auth.exchange_callback(&callback()).await.is_err());
            assert!(!auth.publish_auth(&old.outcome, |_| panic!("Old Start outcome was published")));
            let flow = auth.flow.lock().await;
            assert!(flow.attempt.as_ref().unwrap().state == state && !flow.unknown_issuance);
            let sent = io.sent.lock().unwrap();
            assert!(sent.len() == 3 && matches!(sent[1].route, Route::Cancel));
            assert!(sent[1].body == json!({ "state": "b".repeat(64) }));
        });
    }
    #[test]
    fn start_response_view_increment_checks_last_usable_revision_before_attempt_publication() {
        tauri::async_runtime::block_on(async {
            for overflow in [false, true] {
                let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap();
                let (entered, body) = io.body(Route::Start, 200);
                let a = auth.clone();
                let begin = tauri::async_runtime::spawn(async move { a.begin_login("1", OP1).await });
                entered.await.unwrap();
                {
                    let mut gate = auth.gate();
                    gate.revision = if overflow { TERMINAL - 1 } else { TERMINAL - 2 };
                    gate.view.view_revision = gate.revision.to_string();
                }
                if overflow { io.raw(Route::Cancel, 200, b""); }
                let state = "b".repeat(64);
                assert!(body.send(Ok(serde_json::to_vec(&json!({ "state": state, "expires_in": 120,
                    "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") })).unwrap())).is_ok());
                let begun = begin.await.unwrap().unwrap();
                assert!(begun.outcome.originating_auth_epoch == "1" && begun.outcome.reserved_auth_epoch.as_deref() == Some("2"));
                if overflow {
                    assert!(begun.url.is_none() && begun.outcome.view.auth_epoch == TERMINAL.to_string());
                    assert!(auth.flow.lock().await.attempt.is_none());
                } else {
                    assert!(begun.url.is_some() && begun.outcome.view.view_revision == (TERMINAL - 1).to_string());
                    io.raw(Route::Cancel, 200, b"");
                    let closed = auth.sign_out("2", OP2).await.unwrap();
                    assert!(closed.view.auth_epoch == TERMINAL.to_string() && closed.reserved_auth_epoch.is_none());
                }
                assert!(io.writes.load(Ordering::SeqCst) == 0 && io.request_count() == 2);
            }
        });
    }
    #[test]
    fn exchange_and_mfa_installation_check_epoch_and_view_before_any_store_write() {
        tauri::async_runtime::block_on(async {
            for mfa in [false, true] {
                for epoch_overflow in [false, true] {
                    let (auth, io) = AuthState::fixture(None); auth.view().await.unwrap();
                    let expected = if epoch_overflow { (TERMINAL - 2).to_string() } else { "1".into() };
                    if epoch_overflow {
                        // Inert fixture isolates the checked epoch branch with
                        // low V; it does not claim this is a live counter history.
                        let mut gate = auth.gate(); gate.epoch = TERMINAL - 2;
                        gate.view.auth_epoch = expected.clone();
                    }
                    let state = "b".repeat(64);
                    io.json(Route::Start, 200, json!({ "state": state, "expires_in": 120,
                        "login_url": format!("https://aceteam.ai/auth/app-login?redirect_uri={CALLBACK}&state={state}") }));
                    let begun = auth.begin_login(&expected, OP1).await.unwrap();
                    let pending_epoch = begun.outcome.view.auth_epoch.clone();
                    auth.open_begin(begun, |_| Ok(())).await;
                    if mfa {
                        io.json(Route::Exchange, 202, json!({ "mfa_required": true,
                            "pending_id": "d".repeat(64), "factors": [{ "id": "factor-A" }] }));
                        assert!(auth.exchange_callback(&callback()).await.unwrap().view.mfa_pending);
                    }
                    let (entered, body) = io.body(if mfa { Route::Mfa } else { Route::Exchange }, 200);
                    let a = auth.clone(); let e = pending_epoch.clone();
                    let completion = tauri::async_runtime::spawn(async move {
                        if mfa { a.verify_mfa(&e, OP2, "factor-A", "123456").await }
                        else { a.exchange_callback(&callback()).await }
                    });
                    entered.await.unwrap();
                    if !epoch_overflow {
                        let mut gate = auth.gate(); gate.revision = TERMINAL - 1;
                        gate.view.view_revision = gate.revision.to_string();
                    }
                    io.raw_logout();
                    assert!(body.send(Ok(serde_json::to_vec(&tokens("edge-session")).unwrap())).is_ok());
                    let refused = completion.await.unwrap().unwrap();
                    assert!(refused.view.auth_epoch == TERMINAL.to_string() && !refused.view.signed_in);
                    assert!(refused.error.is_some() && !refused.superseded);
                    assert!(refused.operation_id == if mfa { OP2 } else { OP1 });
                    assert!(refused.originating_auth_epoch == if mfa { pending_epoch } else { expected });
                    assert!(refused.reserved_auth_epoch.is_none() == mfa);
                    assert!(io.writes.load(Ordering::SeqCst) == 0);
                    let flow = auth.flow.lock().await;
                    assert!(flow.candidate_revoked && flow.uncertain_session.as_ref().unwrap().access_token == "fixture-access-edge-session");
                    assert!(flow.session.is_none());
                }
            }
        });
    }
    #[test]
    fn last_usable_credential_revision_rotates_once_then_refuses_next_refresh_before_send() {
        tauri::async_runtime::block_on(async {
            let (auth, io) = AuthState::signed_in_fixture(); auth.view().await.unwrap();
            let origin = auth.capture_origin("1").await.unwrap();
            auth.flow.lock().await.session.as_mut().unwrap().revision = TERMINAL - 2;
            io.raw(Route::Nodes, 401, b""); io.json(Route::Refresh, 200, tokens("last-A"));
            io.json(Route::Nodes, 200, json!([]));
            assert!(auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.is_ok());
            assert!(auth.flow.lock().await.session.as_ref().unwrap().revision == TERMINAL - 1);
            io.raw(Route::Nodes, 401, b"");
            assert!(auth.request_bound(&origin, Method::GET, "/api/fabric/nodes", None).await.is_err());
            assert!(io.request_count() == 4 && auth.gate().phase == Phase::Disabled);
            assert!(!auth.flow.lock().await.unknown_issuance);
            io.raw_logout();
            assert!(auth.sign_out(&TERMINAL.to_string(), OP1).await.unwrap().error.is_none());
            let sent = io.sent.lock().unwrap();
            assert!(sent.iter().filter(|s| matches!(s.route, Route::Refresh)).count() == 1);
            assert!(sent[3].token.as_deref() == Some("fixture-access-last-A"));
            assert!(sent[4].token.as_deref() == Some("fixture-access-last-A"));
            assert!(io.writes.load(Ordering::SeqCst) == 1);
        });
    }
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
            ..AuthFlow::default()
        };
        assert!(take_attempt(&mut flow, &"b".repeat(64)).is_err());
        assert!(flow.attempt.is_some());
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
            format!("citadel://auth/callback?code={}&state={state}", "A".repeat(64)),
            format!("citadel://auth/callback?code={code}&state={}", "B".repeat(64)),
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
    #[test]
    fn epochs_and_operation_ids_refuse_noncanonical_lossy_or_overflow_input() {
        for bad in ["", "0", "01", "-1", "1.0", "1e3", "18446744073709551616", " 1"] {
            assert!(super::parse_epoch(bad, true).is_err());
        }
        assert!(super::parse_epoch("18446744073709551615", false).is_err());
        assert!(super::parse_epoch("18446744073709551615", true).is_ok());
        assert!(!super::valid_operation_id("11111111-1111-4111-C111-111111111111"));
        assert!(!super::valid_operation_id("11111111-1111-1111-8111-111111111111"));
    }
}

