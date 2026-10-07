"""Model-free runtime smoke for faster-whisper's locked PyAV decoder boundary."""

import tempfile
import wave
from pathlib import Path

import torch

from app import _decode_diarization_audio


def main() -> None:
    input_sample_rate = 8_000
    output_sample_rate = 16_000
    with tempfile.TemporaryDirectory(prefix="citadel-whisper-decode-") as temp_dir:
        audio_path = Path(temp_dir) / "stdlib-stereo-8k-silence.wav"
        with wave.open(str(audio_path), "wb") as output:
            output.setnchannels(2)
            output.setsampwidth(2)
            output.setframerate(input_sample_rate)
            output.writeframes(b"\x00\x00\x00\x00" * input_sample_rate)

        audio = _decode_diarization_audio(str(audio_path))
        waveform = audio["waveform"]
        if audio["sample_rate"] != output_sample_rate:
            raise RuntimeError(
                f"unexpected diarization sample rate: {audio['sample_rate']!r}; "
                f"want {output_sample_rate}"
            )
        if tuple(waveform.shape) != (1, output_sample_rate):
            raise RuntimeError(
                f"unexpected diarization waveform shape: {waveform.shape!r}; "
                f"want (1, {output_sample_rate})"
            )
        if waveform.dtype != torch.float32:
            raise RuntimeError(
                f"unexpected diarization waveform dtype: {waveform.dtype!r}; "
                "want torch.float32"
            )
        print(
            f"waveform_shape={tuple(waveform.shape)} "
            f"waveform_dtype={waveform.dtype} sample_rate={audio['sample_rate']} "
            f"input_channels=2 input_sample_rate={input_sample_rate}"
        )


if __name__ == "__main__":
    main()
