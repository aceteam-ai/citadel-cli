# Ollama model residency duration

Refs #1560. For the embedded Ollama container, set `ollama_keep_alive` on the
service in `citadel.yaml`:

```yaml
services:
  - name: ollama
    type: docker
    compose_file: ./services/ollama.yml
    ollama_keep_alive: 60m
```

The value is a non-negative Go duration such as `60m` or `1h30m`, `0` to unload
after a request, or `-1` for indefinite retention. Unitless positive numbers,
other negatives, empty strings, malformed durations and overflow are invalid.
For seconds use a duration such as `3600s`. YAML integer `-1` and `0` also work;
manifest rewrites preserve them as string policy values.

An omitted field retains Ollama's engine default. Citadel supplies an explicit
empty environment value in that case, rather than adopting a shell or Compose
`.env` value as hidden node policy. A configured value replaces every inherited
`OLLAMA_KEEP_ALIVE` duplicate. Other services and unrelated variables are left
alone. This follows the existing `ollama_max_loaded_models` authority pattern.

Command starts, worker SERVICE_START, startup/port-recreate environment builders
and APPLY_DEVICE_CONFIG all carry the setting. Strict starts refuse invalid
policy before engine-side probes or model/resource changes. Existing best-effort
recreate/config builders log the validation error and supply empty rather than
use an ambient value. Both GPU and CPU templates use the same key-only
passthrough. Existing managed template hashes remain registered for refresh.

This field affects a newly created/recreated container's server environment.
Editing the manifest or issuing a no-op start of an already-running container
does not update that process. An authorized operator must apply the changed
container configuration; this source change does not restart a live container.
Native host-managed Ollama has no compose environment to receive this setting,
so Citadel warns that its service environment must be configured separately.

Ollama's [FAQ](https://docs.ollama.com/faq) describes the environment default and
notes that an explicit request `keep_alive` overrides it. The empty default,
negative and zero meanings were also checked against the official
[v0.40.2 KeepAlive implementation](https://github.com/ollama/ollama/blob/v0.40.2/envconfig/config.go#L118-L135).
The image policy is unchanged by this patch. Increasing retention can reduce
repeat cold loads but does not eliminate the first cold load, override an
explicit request value, or reserve memory against scheduler pressure.

No automatic preload, model download, inference request or `model_warming`
protocol change is included. Those optional parts remain tracked in #1560.
