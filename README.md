# Citadel CLI

The `citadel` CLI is the on-premise agent and administrator's toolkit for the AceTeam Sovereign Compute Fabric. It allows you to securely connect your own hardware to your AceTeam account, making your resources available to your private workflows.

## Core Concepts

- **AceTeam:** The cloud-based control plane where you design and manage workflows.
- **Citadel:** The on-premise agent you run on your own hardware (the "node").
- **Nexus:** The secure coordination server (e.g., `nexus.aceteam.ai`) that manages the network, built on Headscale.
- **`citadel.yaml`:** The manifest file that declares a node's identity and the services it provides. This file is **automatically generated** by the `bootstrap` command.

## Installation

Currently, the `citadel` binary must be built from source.

```bash
# This will create binaries for Linux (amd64 and arm64) in the ./build directory.
./build.sh
```

You can then copy the appropriate binary (`./build/linux-amd64/citadel`) to your server.

## Command Reference

### Node Setup & Provisioning

| Command                                                                        | Description                                                                                                                                                                                               |
| :----------------------------------------------------------------------------- | :-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `citadel bootstrap`                                                            | **(Run with sudo)** The primary command for provisioning a new server. It installs dependencies, interactively prompts for configuration, generates all necessary files, and brings the node online.      |
| `citadel bootstrap --authkey <key> --service <name> --node-name <name> --test` | The non-interactive version of bootstrap, perfect for automation. Allows you to specify the service (`vllm`, `ollama`, `llamacpp`, `none`), set the node name, and run a diagnostic test upon completion. |
| `citadel up`                                                                   | Brings a node online using an existing configuration in the current directory. This is typically called automatically by `bootstrap`.                                                                     |
| `citadel down`                                                                 | Stops and removes all services defined in the local `citadel.yaml`.                                                                                                                                       |
| `citadel login`                                                                | Authenticates your machine interactively via a browser. Useful for local development.                                                                                                                     |

### Node Operation & Monitoring

| Command                         | Description                                                                                                                              |
| :------------------------------ | :--------------------------------------------------------------------------------------------------------------------------------------- |
| `citadel status`                | Provides a comprehensive health check dashboard, showing system vitals (CPU, RAM, Disk), GPU status, network status, and service status. |
| `citadel test --service <name>` | Runs a diagnostic test for a specific service to verify its functionality.                                                               |
| `citadel logs <service-name>`   | Streams the logs for a specific service defined in `citadel.yaml`. Supports `-f` to follow the log output.                               |
| `citadel nodes`                 | Connects to the Nexus API and lists all nodes in your compute fabric.                                                                    |
| `citadel run <service-name>`    | Runs a pre-packaged, ad-hoc service without needing a manifest. Great for quick tests.                                                   |
| `citadel version`               | Prints the current version of the CLI.                                                                                                   |

---

## Example Workflow: Provisioning a New GPU Node

This workflow shows how to take a fresh Ubuntu server and turn it into a fully operational Citadel node with a single command.

1.  **Generate an Auth Key:**
    Log in to your Nexus/Headscale admin panel and generate a new, single-use, non-expiring authentication key.

2.  **Bootstrap the Node:**
    Copy the `citadel` binary to the new server. Run the `bootstrap` command with your auth key. It will handle all system setup, configuration, and service deployment.

    **Interactive Example:**

    ```bash
    # The command will prompt you to choose a service and name the node.
    sudo ./citadel bootstrap --authkey tskey-auth-k1A2b3C4d5E6f...
    ```

    **Automated Example:**
    For scripted deployments, you can provide all options as flags. The `--test` flag is highly recommended to verify the deployment.

    ```bash
    # This command will provision a vLLM node named 'gpu-node-01' and run a test.
    sudo ./citadel bootstrap \
      --authkey tskey-auth-k1A2b3C4d5E6f... \
      --service vllm \
      --node-name gpu-node-01 \
      --test
    ```

    After running, `bootstrap` will create a `~/citadel-node` directory containing the generated `citadel.yaml` and service files.

3.  **Verify the Status:**
    Once bootstrap is complete, you can check the node's health at any time.

    ```bash
    # Navigate to the generated directory to manage your node
    cd ~/citadel-node
    ./citadel status
    ```

    You should see `🟢 ONLINE` for the network connection and `🟢 RUNNING` for your configured service. Your node is now ready to accept jobs from the AceTeam control plane.

---

### The `citadel.yaml` Manifest

The `bootstrap` command generates this file for you. It defines the node's identity and the service it runs.

**Example `citadel.yaml` (generated for a vLLM node):**

```yaml
node:
  name: gpu-node-01
  tags:
    - gpu
    - provisioned-by-citadel
services:
  - name: vllm
    compose_file: ./services/vllm.yml
```
