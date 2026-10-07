"""Node-local, offline supervised fine-tuning for the FINETUNE_START contract.

The worker sends one JSON spec on stdin. This program reads only the mounted
dataset and cached base model and writes one adapter or full checkpoint under
/output. Progress is newline-delimited JSON on stdout.
"""

from __future__ import annotations

import json
import math
import os
import sys
from pathlib import Path


DATASET = Path("/data/train.jsonl")
OUTPUT = Path("/output")


def resolve_cached_model(model_id: str) -> str:
    """Resolve one approved model to its pinned local HF snapshot."""
    hf_home = os.environ.get("HF_HOME")
    if not hf_home:
        raise ValueError("HF_HOME is required for offline training")
    repo = Path(hf_home) / "hub" / ("models--" + model_id.replace("/", "--"))
    ref = repo / "refs" / "main"
    try:
        revision = ref.read_text(encoding="utf-8").strip()
    except OSError as exc:
        raise ValueError(f"cached model ref is unavailable for {model_id}") from exc
    if not revision or not revision.isalnum():
        raise ValueError(f"cached model ref is invalid for {model_id}")
    snapshots = (repo / "snapshots").resolve(strict=True)
    snapshot = (snapshots / revision).resolve(strict=True)
    if snapshot.parent != snapshots or not (snapshot / "config.json").is_file():
        raise ValueError(f"cached model snapshot is incomplete for {model_id}")
    return str(snapshot)


def emit(progress_percent: float, current_epoch: int, current_loss: float) -> None:
    if not math.isfinite(current_loss):
        raise ValueError("training loss must be finite")
    print(
        json.dumps(
            {
                "type": "progress",
                "progress_percent": max(0.0, min(100.0, progress_percent)),
                "current_epoch": current_epoch,
                "current_loss": current_loss,
            }
        ),
        flush=True,
    )


def load_rows(tokenizer: object) -> list[dict[str, list[int]]]:
    rows: list[dict[str, list[int]]] = []
    with DATASET.open(encoding="utf-8") as dataset:
        for number, raw in enumerate(dataset, 1):
            if not raw.strip():
                continue
            value = json.loads(raw)
            if not isinstance(value, dict):
                raise ValueError(f"dataset row {number} must be an object")
            text = value.get("text")
            if text is None and isinstance(value.get("messages"), list):
                text = tokenizer.apply_chat_template(  # type: ignore[attr-defined]
                    value["messages"], tokenize=False, add_generation_prompt=False
                )
            if not isinstance(text, str) or not text.strip():
                raise ValueError(f"dataset row {number} requires text or messages")
            rows.append({"text": text})
    if not rows:
        raise ValueError("dataset has no training rows")
    return rows


def main() -> None:
    spec = json.loads(sys.stdin.readline())
    model_id = spec["model"]
    if model_id not in ("Qwen/Qwen3-8B", "Qwen/Qwen3-0.6B"):
        raise ValueError("base model is not approved")
    method = spec["method"]
    if method not in ("lora", "qlora", "full"):
        raise ValueError("unsupported training method")
    hp = spec["hyperparameters"]
    model_path = resolve_cached_model(model_id)

    # Imports live inside main so the contract test can load this module with
    # ordinary Python and no GPU or heavy ML wheels.
    import torch
    from datasets import Dataset
    if method != "full":
        # Import Unsloth before transformers/peft so its patches apply. Passing
        # a resolved snapshot path also prevents its repository probe from
        # attempting a network request while HF_HUB_OFFLINE=1.
        from unsloth import FastLanguageModel

    from transformers import (
        AutoModelForCausalLM,
        AutoTokenizer,
        DataCollatorForLanguageModeling,
        Trainer,
        TrainerCallback,
        TrainingArguments,
    )

    if method == "full":
        tokenizer = AutoTokenizer.from_pretrained(model_path, local_files_only=True)
        model = AutoModelForCausalLM.from_pretrained(
            model_path, local_files_only=True, torch_dtype=torch.float16
        )
    else:
        model, tokenizer = FastLanguageModel.from_pretrained(
            model_name=model_path,
            max_seq_length=hp["max_seq_length"],
            dtype=torch.float16,
            load_in_4bit=(method == "qlora"),
        )
        model = FastLanguageModel.get_peft_model(
            model,
            r=hp["lora_r"],
            lora_alpha=hp["lora_alpha"],
            lora_dropout=hp["lora_dropout"],
            target_modules=[
                "q_proj", "k_proj", "v_proj", "o_proj",
                "gate_proj", "up_proj", "down_proj",
            ],
            use_gradient_checkpointing="unsloth",
            random_state=3407,
        )

    if tokenizer.pad_token is None:
        tokenizer.pad_token = tokenizer.eos_token
    rows = load_rows(tokenizer)
    data = Dataset.from_list(rows).map(
        lambda batch: tokenizer(
            batch["text"], truncation=True, max_length=hp["max_seq_length"]
        ),
        batched=True,
        remove_columns=["text"],
    )

    class Progress(TrainerCallback):
        def on_log(self, args, state, control, logs=None, **kwargs):
            if logs and "loss" in logs:
                emit(
                    100.0 * state.global_step / max(1, state.max_steps),
                    min(hp["epochs"], int(state.epoch or 0) + 1),
                    float(logs["loss"]),
                )

    arguments = TrainingArguments(
        output_dir=str(OUTPUT),
        num_train_epochs=hp["epochs"],
        learning_rate=hp["learning_rate"],
        per_device_train_batch_size=hp["batch_size"],
        logging_steps=1,
        save_strategy="no",
        report_to="none",
        fp16=True,
        gradient_checkpointing=True,
        remove_unused_columns=False,
    )
    trainer = Trainer(
        model=model,
        args=arguments,
        train_dataset=data,
        data_collator=DataCollatorForLanguageModeling(tokenizer=tokenizer, mlm=False),
        callbacks=[Progress()],
    )
    trainer.train()
    model.save_pretrained(OUTPUT)
    tokenizer.save_pretrained(OUTPUT)
    emit(100.0, hp["epochs"], float(trainer.state.log_history[-1].get("loss", 0.0)))


if __name__ == "__main__":
    main()
