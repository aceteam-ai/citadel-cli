# Citadel CLI TODO

## Core Features

- [x] `bootstrap` command for node provisioning
- [x] `up` command to start services from manifest
- [x] `down` command to stop services from manifest
- [x] `status` command for node health check
- [x] `logs` command to stream service logs
- [x] `run` command for pre-packaged services (ollama, etc.)
- [ ] `agent` command to listen for jobs from Nexus
- [ ] `nodes` command to list nodes from Nexus API

## Next Steps

1.  **Implement Nexus API Client:** Create a new `internal/nexus` package. This will be a Go client for making authenticated API calls to the Nexus control plane. It will be used by `nodes` and `agent`.
2.  **Implement `nodes` command:** Use the new Nexus API client to fetch and display a list of nodes. This will be the first real test of the API client.
3.  **Flesh out `agent` command:** Replace the placeholder loop with a real gRPC client that connects to Nexus, listens for jobs, executes them, and reports status.

## Polish & DX

- [ ] `init` command to generate a template `citadel.yaml`.
- [ ] `logout` command.
- [ ] Integrate `viper` for configuration management.
- [ ] Add more pre-packaged services to the `run` command (lmstudio, llamacpp).

Alright, let's pop the case open and see what we've got. Today's October 1st, 2025, and we've got a good frame here, but we're missing the core components to make this thing really hum. You've built a solid scaffolding with `bootstrap`, `login`, and `up`. That's like having the chassis, power supply, and the OS installer ready.

Here's a breakdown of what's missing to turn this into a fully functional system, ordered from most critical to "nice-to-have."

### Tier 1: Critical Missing Components (The "Motherboard & CPU")

#### 1. The Background Agent (The "CPU")

This is the single most important missing piece. The `up` command has a `TODO` for it, and it's the entire reason this tool exists. The CLI's job is to set up the environment, but the **agent's** job is to do the actual work.

**What it needs to do:**

- **Connect to Nexus:** After `citadel up` finishes, it should spawn a long-running process (the agent). This agent needs to establish a persistent connection to the Nexus control plane (likely via its Tailscale IP/MagicDNS name). This could be a WebSocket, gRPC stream, or long-polling HTTP connection.
- **Listen for Jobs:** It will receive commands from Nexus, like "run this ML inference job," "process this dataset," or "update your configuration."
- **Execute Jobs:** It needs a job execution engine. This could involve:
  - Running `docker run` or `docker compose` with specific parameters/environment variables provided by the job.
  - Executing shell scripts.
  - Managing data volumes.
- **Report Status:** It must report back to Nexus: "Job received," "Job running," "Job completed successfully," "Job failed with error." It should also stream logs back to Nexus so users can see job output in the AceTeam UI.

This is a significant piece of software engineering and is the core value proposition.

#### 2. Implementation of Placeholder Commands

Several commands are just stubs. We need to wire them up to be useful tools for the administrator.

- **`down.go`:** The logical opposite of `up`.

  - It should read `citadel.yaml`.
  - For each service, run `docker compose -f <compose_file> down`.
  - Optionally, it could run `sudo tailscale logout` or `tailscale down` to take the node completely off the network.

- **`status.go`:** This should be a health check dashboard.

  - Run `tailscale status --json` to get network connectivity info (IP address, connection status).
  - Read `citadel.yaml` and for each service, run `docker compose -f <compose_file> ps` to show if the containers are running.
  - Check if the background agent process is running and connected to Nexus.

- **`logs.go`:** Essential for debugging.

  - It should read `citadel.yaml` to know about the services.
  - It should take an optional `[service-name]` argument.
  - It should execute `docker compose -f <compose_file> logs [service-name]`.
  - It needs to support common flags like `-f` (follow) and `--tail N`.

- **`nodes.go`:** This is the first command that needs to talk to the Nexus API.
  - It should make an authenticated API call to a Nexus endpoint (e.g., `https://nexus.aceteam.ai/api/v1/nodes`).
  - It will need an API client (see next point).
  - It should list all nodes in the user's network, showing their name, Tailscale IP, status (online/offline), and tags.

### Tier 2: Foundational Enhancements (The "Wiring & Cooling")

#### 3. Nexus API Client

The `nodes` command and the background agent _both_ need to communicate with the Nexus control plane. Instead of writing raw HTTP requests everywhere, you should build a small, reusable Go client package.

- Create a new package, e.g., `internal/nexus`.
- Define methods like `nexus.GetNodes()`, `nexus.GetJob()`, `nexus.UpdateJobStatus()`.
- Handle authentication. Since you're using Tailscale, the best way is likely using Tailscale's identity headers or mutual TLS to authenticate the node to the Nexus API, which would also be on the Tailnet. This avoids managing separate API keys.

#### 4. Configuration Management (`viper`)

Right now, `nexusURL` is a global flag. This is good, but for a real tool, you need a config file. The `root.go` already has a placeholder for it.

- Integrate `viper` in `root.go`'s `init()` function.
- Read from `/etc/citadel/config.yaml`, `$HOME/.citadel.yaml`, and the current directory.
- This allows users to set `nexus: https://...` once and not type it for every command.
- It's also the perfect place to store state, like the agent's unique ID after its first registration.

### Tier 3: User Experience & Polish (The "Case & RGB")

#### 1. A `citadel init` Command

Instead of telling users to create a `citadel.yaml` by hand, give them a command to generate a template.

```bash
citadel init
```

This would ask a few questions ("What do you want to name this node?") and create a well-commented `citadel.yaml` file.

#### 2. A `citadel logout` Command

You have `login`, you need `logout`. This would simply be a wrapper around `sudo tailscale logout`.

#### 3. Better Error Handling and Validation

- In `up.go`, what happens if `citadel.yaml` doesn't exist? It panics. It should print a friendly error: "`citadel.yaml` not found. Please create one or run `citadel init`."
- What if a `compose_file` listed in the manifest doesn't exist? Validate all paths before trying to execute commands.
- The `bootstrap` command is powerful. Add more checks and clearer output. Use a spinner or progress indicator for long-running steps like `apt-get update`.

#### 4. Security Hardening

- The `bootstrap` command passes the `--authkey` on the command line, which means it gets stored in shell history (`.bash_history`). You should support reading the key from an environment variable (`CITADEL_AUTHKEY`) or a file to improve security.

### Prioritized Implementation Plan:

1.  **Implement `down`, `status`, `logs`:** These are straightforward and provide immediate utility for managing the services started by `up`.
2.  **Design and Build the Nexus API:** You can't build the agent or the `nodes` command without knowing what the API looks like.
3.  **Build the Nexus API Client:** Create the reusable Go package.
4.  **Implement the Background Agent:** This is the core feature. Start simple: connect, receive a "hello world" job, print it, and report success.
5.  **Implement `nodes`:** This will be the first real test of your Nexus API client.
6.  **Add `init` and `logout` commands:** These are great for improving the user experience.
7.  **Integrate `viper`:** Refactor to pull configuration from files.
8.  **Add Tests:** You have no tests. Start adding unit tests for functions like `readManifest` and then look into integration tests for the commands.

You've got a great start. Now it's time to install the main components and wire everything together. Let's get building.
