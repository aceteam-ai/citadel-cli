# Citadel CLI

The `citadel` CLI is the on-premise agent and administrator's toolkit for the AceTeam Sovereign Compute Fabric. It allows you to securely connect your own hardware to your AceTeam account, making your resources available to your private workflows.

## Core Concepts

- **AceTeam:** The cloud-based control plane where you design and manage workflows.
- **Citadel:** The on-premise agent you run on your own hardware (the "node").
- **Nexus:** The secure coordination server (e.g., `nexus.aceteam.ai`) that manages the network.
- **`citadel.yaml`:** The manifest file that declares a node's identity and the services it provides. This file is **automatically generated** by the `init` command.

## Installation

### From a Release (Recommended)

1.  Go to the [**Releases Page**](https://github.com/aceboss/citadel-cli/releases).
2.  Download the latest `.tar.gz` archive for your architecture (e.g., `citadel_vX.Y.Z_linux_amd64.tar.gz`).
3.  Extract the archive and place the `citadel` binary in your `PATH`.

    ```bash
    tar -xvf citadel_vX.Y.Z_linux_amd64.tar.gz
    sudo mv citadel /usr/local/bin/
    ```

### Building from Source

If you need to build from the latest source code:

```bash
# This will create binaries for Linux (amd64 and arm64) in the ./build directory.
./build.sh
```

## Command Reference

### Node Setup & Provisioning

| Command                                                                   | Description                                                                                                                                                                                            |
| :------------------------------------------------------------------------ | :----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `citadel init`                                                            | **(Run with sudo)** The primary command for provisioning a new server. It installs dependencies, interactively prompts for configuration, generates all necessary files, and brings the node online.   |
| `citadel init --authkey <key> --service <name> --node-name <name> --test` | The non-interactive version of `init`, perfect for automation. Allows you to specify the service (`vllm`, `ollama`, `llamacpp`, `none`), set the node name, and run a diagnostic test upon completion. |
| `citadel up`                                                              | Brings a node online using an existing configuration. This is typically called automatically by `init`.                                                                                                |
| `citadel down`                                                            | Stops and removes all services defined in the local `citadel.yaml`.                                                                                                                                    |
| `citadel login`                                                           | **(Run with sudo)** Connects the machine to the network. It's intelligent: if already online, it does nothing. If offline, it provides interactive prompts to connect via authkey or a browser.        |

### Node Operation & Monitoring

| Command                         | Description                                                                                                                                  |
| :------------------------------ | :------------------------------------------------------------------------------------------------------------------------------------------- |
| `citadel status`                | Provides a comprehensive health check dashboard, showing the CLI version, system vitals (CPU, RAM, Disk), GPU status, network, and services. |
| `citadel test --service <name>` | Runs a diagnostic test for a specific service to verify its functionality.                                                                   |
| `citadel logs <service-name>`   | Streams the logs for a specific service defined in `citadel.yaml`. Supports `-f` to follow the log output.                                   |
| `citadel nodes`                 | Connects to the Nexus API and lists all nodes in your compute fabric.                                                                        |
| `citadel run <service-name>`    | Runs a pre-packaged, ad-hoc service without needing a manifest. Great for quick tests.                                                       |
| `citadel version`               | Prints the current version of the CLI.                                                                                                       |

---

## Example Workflow: Provisioning a New GPU Node

This workflow shows how to take a fresh Ubuntu server and turn it into a fully operational Citadel node with a single command.

1.  **(Optional) Generate an Auth Key:**
    For automated deployments, log in to your Nexus admin panel and generate a new, single-use, non-expiring authentication key. For interactive setup, you can skip this and log in via your browser.

2.  **Initialize the Node:**
    Copy the `citadel` binary to the new server and run the `init` command. It will handle all system setup, configuration, and service deployment.

    **Interactive Example:**

    ```bash
    # The command will guide you through selecting a service, naming the node,
    # and choosing a connection method (browser or authkey).
    sudo ./citadel init
    ```

    **Automated Example:**
    For scripted deployments, you can provide all options as flags. The `--test` flag is highly recommended to verify the deployment.

    ```bash
    # This command will provision a vLLM node named 'gpu-node-01' and run a test.
    sudo ./citadel init \
      --authkey tskey-auth-k1A2b3C4d5E6f... \
      --service vllm \
      --node-name gpu-node-01 \
      --test
    ```

    After running, `init` will create a `~/citadel-node` directory containing the generated `citadel.yaml` and service files.

3.  **Verify the Status:**
    Once initialization is complete, you can check the node's health at any time.

    ```bash
    # Navigate to the generated directory to manage your node
    cd ~/citadel-node
    ./citadel status
    ```

    You should see `🟢 ONLINE` for the network connection and `🟢 RUNNING` for your configured service. Your node is now ready to accept jobs from the AceTeam control plane.

---

### The `citadel.yaml` Manifest

The `init` command generates this file for you. It defines the node's identity and the service it runs.

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
