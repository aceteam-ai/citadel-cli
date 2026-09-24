# Node-local fine tuning

`FINETUNE_START` runs the local image `citadel-finetune:local`. Build it on the
target architecture before enabling the worker:

```sh
docker build -t citadel-finetune:local services/finetune
```

The worker accepts only the approved Qwen3 base models and a workspace-relative
`dataset_node_path` on the same pinned node. Dataset rows are JSONL objects with
either `text` or chat `messages`. The base model must already exist in the
node's Hugging Face cache: the training container has no network, receives the
dataset and cache read-only, and writes only to the job's adapter directory
under the node config volume.

The image uses PyTorch 2.5.1 with CUDA 12.4 and pins xformers to the matching
PyTorch release. LoRA and QLoRA use Unsloth; full training uses Transformers.
The existing device-authenticated API companion must be available for
API-proxy workers. Do not mark the issue or PR ready until a real RTX 3090
toy run demonstrates progress, cancellation, preemption, and restoration.
