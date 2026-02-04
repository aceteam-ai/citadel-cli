---
sidebar_position: 1
title: Managing Services
---

# Managing Services

Services are AI inference engines that run as Docker containers on your node. Citadel manages their lifecycle through Docker Compose, with compose files embedded directly in the binary -- no external files to download or maintain.

## Supported Services

| Service | Description | GPU Required |
|---------|-------------|--------------|
| **vLLM** | High-throughput LLM inference server | Yes (NVIDIA) |
| **Ollama** | Easy-to-use model runner | No (GPU optional) |
| **llama.cpp** | Lightweight CPU/GPU inference | Yes (NVIDIA) |
| **LM Studio** | Desktop-friendly model server | No (GPU optional) |
| **Extraction** | Generic extraction service | No |

## Starting Services

Start all services defined in your `citadel.yaml` manifest:

```bash
citadel run
```

Start a specific service:

```bash
citadel run ollama
```

Services are started using `docker compose` under the hood. Each service runs in its own project namespace (e.g., `citadel-vllm`, `citadel-ollama`) to keep containers organized and isolated.

## Stopping Services

Stop all services:

```bash
citadel stop
```

Stop a specific service:

```bash
citadel stop ollama
```

## Restarting Services

Restart all services (stops and then starts them):

```bash
citadel run --restart
```

## Viewing Logs

Stream logs from a running service:

```bash
citadel logs vllm -f
```

The `-f` flag follows the log output in real time, similar to `docker compose logs -f`.

## Testing Services

Run diagnostic tests against a service to verify it is healthy and responding:

```bash
citadel test --service vllm
```

This sends test requests to the service endpoint and reports whether inference is working correctly.

## GPU Requirements

vLLM and llama.cpp require the NVIDIA Container Toolkit and a properly configured Docker runtime. The NVIDIA runtime must be set as the default in `/etc/docker/daemon.json`:

```json
{
  "default-runtime": "nvidia",
  "runtimes": {
    "nvidia": {
      "path": "nvidia-container-runtime",
      "runtimeArgs": []
    }
  }
}
```

If you used `sudo citadel init --provision`, this configuration was applied automatically. For manual setups, install the [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html) and restart Docker after updating the daemon configuration.

Ollama, LM Studio, and Extraction run without GPU access, though Ollama will use a GPU if one is available.

## How It Works

Docker Compose files for each supported service are embedded in the Citadel binary using Go's `embed` package. When you run `citadel run`, the CLI extracts the appropriate compose file and invokes `docker compose` to manage the container lifecycle. This means you never need to manage compose files yourself -- everything is self-contained in the binary.
