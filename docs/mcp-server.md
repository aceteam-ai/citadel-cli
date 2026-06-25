# AceTeam MCP Server

The AceTeam MCP server lets AI coding agents (Claude Code, Cursor, Windsurf, etc.) manage GPU infrastructure, run inference, execute code on remote nodes, and interact with the full AceTeam platform.

## Setup

```bash
# Install Citadel CLI
curl -fsSL https://get.aceteam.ai/citadel | bash

# Authenticate
citadel init

# Add to Claude Code
claude mcp add aceteam -- citadel mcp
```

Or with an API key:

```bash
claude mcp add aceteam -e ACETEAM_API_KEY=act_xxx -- citadel mcp
```

## How It Works

`citadel mcp` starts a local stdio MCP server that proxies JSON-RPC messages to the AceTeam backend. It uses your existing Citadel credentials (from `citadel init`) or an API key.

## Tools

### Compute Fabric

| Tool | Description |
|------|-------------|
| `fabric_list_nodes` | List GPU nodes in your organization or the public marketplace |
| `fabric_node_status` | Get detailed node status: GPUs, VRAM, running services, temperature |
| `fabric_dispatch_job` | Submit an inference or compute job to the fabric |
| `fabric_embed_text` | Embed `texts: []string` on a node's TEI service → vectors + token usage. Args: `node_id`, `texts`, optional `model_id`, optional `dimensions` (Matryoshka; defaults to native). Dispatches an `embedding` job handled node-side by the TEI `/v1/embeddings` route. |
| `fabric_list_models` | Browse available models across all nodes |
| `fabric_node_earnings` | View earnings for a specific node |
| `fabric_earnings_summary` | Aggregate earnings across all nodes |

### Services

| Tool | Description |
|------|-------------|
| `service_start` | Start an inference engine (vLLM, Ollama, llama.cpp) on a node |
| `service_stop` | Stop a running service |
| `service_status` | Check service health and resource usage |

### Code Execution

| Tool | Description |
|------|-------------|
| `code_read` | Read a file from a remote Citadel node |
| `code_write` | Write a file to a remote node |
| `code_edit` | Edit a file on a remote node (find and replace) |
| `code_list` | List files in a directory on a remote node |
| `code_search` | Search for text across files on a remote node |

### Terminal

| Tool | Description |
|------|-------------|
| `terminal_exec` | Execute a shell command on a remote node |
| `terminal_list_nodes` | List nodes available for terminal access |

### Network (Nexus)

| Tool | Description |
|------|-------------|
| `nexus_list_nodes` | List all nodes on the VPN mesh |
| `nexus_get_node` | Get details for a specific node |
| `nexus_generate_authkey` | Generate an auth key for onboarding new nodes |
| `nexus_expire_node` | Expire a node's network key |
| `nexus_delete_node` | Remove a node from the network |

### Desktop

| Tool | Description |
|------|-------------|
| `desktop_screenshot` | Capture a screenshot from a node's display |

### Billing (ACET)

| Tool | Description |
|------|-------------|
| `acet_balance` | Check your ACET token balance |
| `acet_purchase` | Purchase ACET tokens |
| `acet_history` | View transaction history |

### Agents

| Tool | Description |
|------|-------------|
| `create_agent` | Create an AI agent |
| `list_agents` | List all agents in your organization |
| `get_agent` | Get agent details and configuration |
| `update_agent` | Update agent settings |
| `delete_agent` | Delete an agent |
| `chat_with_agent` | Send a message to an agent |

### Knowledge Base

| Tool | Description |
|------|-------------|
| `search_knowledge_base` | Semantic search across uploaded documents |
| `list_collections` | List document collections |
| `upload_document` | Upload a document for RAG |
| `list_documents` | List documents in a collection |

### Integrations

| Tool | Description |
|------|-------------|
| `email_search` | Search emails across connected Gmail accounts |
| `email_read` | Read a specific email |
| `email_draft` | Create a draft email |
| `email_send` | Send an email |
| `slack_send_message` | Send a Slack message |
| `slack_read_messages` | Read messages from a Slack channel |
| `calendar_list_events` | List calendar events |
| `calendar_create_event` | Create a calendar event |
| `drive_read_file` | Read a file from Google Drive |
| `drive_search_files` | Search Google Drive |

## Examples

### Deploy a model to your GPU

```
You: Start vLLM with Llama 3 8B on my node

Agent calls: fabric_list_nodes → service_start(node_id, service="vllm", model="meta-llama/Llama-3-8B")
```

### Check GPU utilization

```
You: How are my GPUs doing?

Agent calls: fabric_list_nodes → fabric_node_status(node_id)
Returns: GPU model, VRAM usage, temperature, running services
```

### Run a command on a remote node

```
You: Check disk space on my inference server

Agent calls: terminal_exec(node_id, command="df -h")
```

## Authentication

Three methods, checked in order:

1. `--api-key` flag: `citadel mcp --api-key act_xxx`
2. `ACETEAM_API_KEY` environment variable
3. Citadel config file (from `citadel init`)

## Remote MCP (no CLI required)

AceTeam also offers a hosted MCP endpoint at `https://aceteam.ai/api/mcp/aceteam/mcp` that requires OAuth authentication. Use this when you can't install the CLI.
