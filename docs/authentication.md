# Citadel CLI Authentication

The `citadel` CLI needs to authenticate with a control plane to register a new node to the network fabric. By default, it uses the managed AceTeam AI control plane, but it is fully configurable to work with a self-hosted environment.

## Authentication Flow

Citadel uses a browser-based authentication flow inspired by the **OAuth 2.0 Device Authorization Grant**. This provides a secure and user-friendly way to provision new nodes without manually handling API keys in the terminal.

When you run `citadel init`, the following happens:

1.  The CLI contacts the configured Authentication Service to start the login process.
2.  The service provides a unique code and a verification URL.
3.  The CLI displays these to you. You open the URL in a browser, log in, and enter the code.
4.  Once you approve the request in your browser, the CLI securely obtains a single-use provisioning key from the service.
5.  The CLI uses this key to register itself with the Headscale (Nexus) instance.

## Configuration

You can configure the CLI to point to your own self-hosted control plane and Nexus instance using environment variables.

*   `CITADEL_AUTH_HOST`: The base URL for your Authentication Service.
    *   **Default:** `https://aceteam.ai`
*   `CITADEL_NEXUS_HOST`: The URL for your Headscale instance.
    *   **Default:** `https://nexus.aceteam.ai`

**Example:**
```sh
export CITADEL_AUTH_HOST="https://my-control-plane.com"
export CITADEL_NEXUS_HOST="https://my-headscale.com"
sudo -E citadel init
```
*Note: `sudo -E` is required to preserve the environment variables for the root user.*

## Implementing a Custom Authentication Service

If you wish to build your own control plane compatible with `citadel-cli`, you must implement the following API endpoints.

### 1. Start Device Authorization

The CLI will make a request to this endpoint to begin the flow.

*   **Endpoint:** `POST ${CITADEL_AUTH_HOST}/api/fabric/device-auth/start`
*   **Request Body:** (empty)
*   **Success Response (200 OK):**
    ```json
    {
      "device_code": "a-long-secret-code-for-the-cli-to-poll-with",
      "user_code": "ABCD-1234",
      "verification_uri": "https://your-service.com/device",
      "interval": 5
    }
    ```
    *   `device_code`: A high-entropy string the CLI will use in the next step.
    *   `user_code`: A short, user-friendly code to be entered in the browser.
    *   `verification_uri`: The URL the user must visit.
    *   `interval`: The recommended polling interval in seconds.

### 2. Poll for Token

The CLI will poll this endpoint at the specified `interval` until the user authenticates.

*   **Endpoint:** `POST ${CITADEL_AUTH_HOST}/api/fabric/device-auth/token`
*   **Request Body:**
    ```json
    {
      "device_code": "the-long-secret-code-from-the-previous-step"
    }
    ```
*   **Pending Response (400 Bad Request):**
    *   When the user has not yet approved the request.
    ```json
    {
      "error": "authorization_pending"
    }
    ```
*   **Success Response (200 OK):**
    *   When the user has approved the request. Your service should generate a single-use Headscale pre-authentication key and return it here.
    ```json
    {
      "access_token": "hs_preauth_key_for_headscale_registration"
    }
    ```

The `citadel-cli` will then use this `access_token` to complete its registration with the `CITADEL_NEXUS_HOST`.