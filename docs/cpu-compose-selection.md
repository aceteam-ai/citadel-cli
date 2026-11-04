# CPU compose selection

On Linux, Citadel samples its existing NVIDIA detector when it materializes or
starts the embedded Ollama and llama.cpp services. A positive GPU count keeps
the existing GPU compose bytes. A zero count, including a failed detector,
selects the existing CPU template also used on macOS. llama.cpp uses the
`:server` image instead of `:server-cuda`; both CPU templates omit the NVIDIA
device reservation. Ports, volumes, model interpolation and environment policy
stay the same. CUDA-only engines such as vLLM and SGLang retain their templates.

This selection also applies at initial configuration and the versioned boot
refresh. Existing manifest and module starts recheck the selected flavor before
an already-running shortcut. The file change itself never restarts a container;
the normal start or refresh path still owns runtime actions.

A later start can change a known Citadel compose from GPU to CPU or back. The
start path only replaces the GPU flavor of a byte-recognized embedded template,
including this node-directory override's expected container name. Operator edits
and foreign namespaces are preserved. Same-flavor historical upgrades remain
the versioned refresh sweep's job. Custom manifest paths are left alone.

Replacements use an atomic rename with mode 0600 and update an existing managed
stamp's service hash without changing its version or other services. An error
updating the stamp is returned even if the compose rename succeeded; a repeat
with the already-selected template can repair that hash. This does not claim
crash durability across the two file updates. Existing service reservation and
fine-tune admission guards remain authoritative.
