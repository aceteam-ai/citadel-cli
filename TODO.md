# Citadel CLI TODO

## Completed Features ✅

- [x] `init` command for node provisioning (network-only by default, `--provision` for full setup)
- [x] `work` command - unified worker (starts services + Redis job consumer)
- [x] `run` / `stop` commands for service management
- [x] `status` command for node health check
- [x] `logs` command to stream service logs
- [x] `login` / `logout` commands for network authentication
- [x] `update` command with A/B rollback strategy
- [x] `terminal-server` for WebSocket-based remote access
- [x] `nodes` command to list nodes from Nexus API
- [x] Device authorization flow (RFC 8628)
- [x] Redis Streams job consumer
- [x] Redis Pub/Sub for real-time config updates
- [x] Heartbeat reporting to AceTeam API
- [x] Cross-platform support (Linux, macOS, Windows)

## In Progress 🚧

- [ ] VLLM Inference job handler (code exists, needs testing)

## Future Enhancements 📋

- [ ] Windows winget package publication
- [ ] macOS Homebrew formula
- [ ] Job streaming improvements (real-time output)
- [ ] GPU memory monitoring in status
- [ ] Service health checks with automatic restart
