"""Opt-in GPU backend: Cohere Transcribe via Hugging Face transformers.

The model (`CohereLabs/cohere-transcribe-03-2026` by default) is licensed
Apache-2.0 per its Hugging Face model card, and GATED on the Hub: downloading it
needs an `HF_TOKEN` from an account that accepted its terms. This project ships
only the code that loads it, never the weights. See `server/stt/README.md`.

Selected by `STT_BACKEND=cohere` (the compose `gpu` profile). Needs the
`Dockerfile.cohere` image (CUDA torch + transformers >= 5).

Environment:

  STT_MODEL            default `CohereLabs/cohere-transcribe-03-2026`
  STT_MAX_NEW_TOKENS   default 256 — the token cap `truncated` reports on
  STT_SCORES           per-token scores (feeds avg_logprob / no_speech_prob),
                       default on; `0` turns them off without a rebuild
  HF_TOKEN             required to download a gated model
"""

from __future__ import annotations

import logging
import math
import re
import time

import numpy as np

from .base import Backend, BackendRequestError, Transcript, env

log = logging.getLogger("stt.cohere")

# `CohereAsrProcessor.get_decoder_prompt_ids` accepts exactly these BARE codes
# and raises ValueError for anything else (e.g. the BCP-47 tag `en-US`). The
# processor is still the authority: `transcribe` also turns its ValueError into
# a 400, so a model update that moves the set degrades only the error message.
SUPPORTED_LANGUAGES = frozenset(
    {"ar", "de", "el", "en", "es", "fr", "it", "ja", "ko", "nl", "pl", "pt", "vi", "zh"}
)

# Matched against the tokenizer's ADDED vocabulary only (the special tokens),
# never the full vocab, where it would hit the English word "silence".
NO_SPEECH_TOKEN_PATTERN = re.compile(r"no[\W_]*speech|nospeech|silen(?:ce|t)", re.I)


def find_no_speech_token(added_vocab: dict) -> int | None:
    """Id of a dedicated no-speech token, or None if the model has none.

    Measured on the default model: `<|nospeech|>` is id 1 and the only added
    token this pattern matches.
    """
    for token, tid in sorted(added_vocab.items()):
        if NO_SPEECH_TOKEN_PATTERN.search(str(token)):
            return int(tid)
    return None


def eos_id_set(raw) -> frozenset:
    """`eos_token_id` normalised to a set of ints — it may be int, list or None."""
    if raw is None:
        return frozenset()
    if isinstance(raw, (list, tuple, set, frozenset)):
        return frozenset(int(t) for t in raw if t is not None)
    return frozenset({int(raw)})


def finish_reason_of(rows, eos: frozenset) -> str:
    """"stop" | "length" | "unknown", read from the TOKENS.

    🔴 "Is an EOS present in the row", not "is it last": long audio is chunked,
    and every row shorter than the longest is right-padded past its own EOS.
    🔴 "unknown" when there is no EOS id to look for — reporting "length" would
    stamp `truncated: true` on every transcript.
    """
    if not eos:
        return "unknown"
    for row in rows:
        if not any(int(t) in eos for t in row):
            return "length"
    return "stop"


def mean_logprob(token_logprobs) -> float | None:
    """Mean per-token logprob ignoring non-finite entries (padding is -inf).
    None when nothing finite survives — never a number that looks measured."""
    if token_logprobs is None:
        return None
    vals = [float(v) for v in token_logprobs if math.isfinite(float(v))]
    if not vals:
        return None
    return sum(vals) / len(vals)


def no_speech_prob_of(step_probs) -> float | None:
    """Largest P(no-speech token) over the generated steps, or None.

    🔴 An exact zero at every step is the signature of a MASKED token, not an
    observation about the audio, so it is reported as None ("no signal"), never
    as 0.0 ("definitely speech"). A genuine tiny probability is evidence and
    survives — hence `== 0.0` and not a tolerance.
    """
    if not step_probs:
        return None
    vals = [float(p) for p in step_probs if math.isfinite(float(p))]
    if not vals:
        return None
    best = max(vals)
    return None if best == 0.0 else best


def _scores_on() -> bool:
    return env("STT_SCORES", "1").strip().lower() not in {"0", "false", "no", "off", ""}


class CohereBackend(Backend):
    name = "cohere"
    # Measured by the maintainer on a private corpus of real dictation with this
    # model: 8e-9 .. 1.5e-8 separated clean speech from fabrications with no
    # errors either way, and 1e-8 is the centre of that band. Recalibrate on
    # your own voice before relying on it.
    default_no_speech_max = "1e-8"
    # Deliberately unconfigured: avg_logprob discriminates too, but not by a
    # margin a four-clip sample can set a threshold from.
    default_avg_logprob_min = ""

    def __init__(self) -> None:
        super().__init__()
        self.model = env("STT_MODEL", "CohereLabs/cohere-transcribe-03-2026")
        self.max_new_tokens = int(env("STT_MAX_NEW_TOKENS", "256"))
        self.scores = _scores_on()
        self.languages = SUPPORTED_LANGUAGES
        self._model = None
        self._processor = None
        self._no_speech_token = None

    def load(self) -> None:
        import torch  # lazy: only the GPU image has it
        from transformers import AutoModelForSpeechSeq2Seq, AutoProcessor

        t0 = time.time()
        self.device = "cuda" if torch.cuda.is_available() else "cpu"
        if self.device == "cuda":
            log.info("gpu: %s", torch.cuda.get_device_name(0))
            # Prove a real tensor op works on this GPU before serving.
            x = torch.ones(8, device="cuda") @ torch.ones(8, device="cuda")
            log.info("cuda tensor op ok: sum=%s", float(x.sum()))
        dtype = torch.bfloat16 if self.device == "cuda" else torch.float32
        processor = AutoProcessor.from_pretrained(self.model)
        model = AutoModelForSpeechSeq2Seq.from_pretrained(self.model, torch_dtype=dtype).to(self.device)
        model.eval()
        try:
            self._no_speech_token = find_no_speech_token(processor.tokenizer.get_added_vocab())
        except Exception as e:  # noqa: BLE001 - a diagnostic must never fail a load
            log.warning("could not inspect the added vocabulary for a no-speech token: %s", e)
        self._model, self._processor = model, processor
        self.ready = True
        log.info(
            "model %s loaded on %s in %.1fs (scores=%s, no_speech_token=%s, eos=%s)",
            self.model, self.device, time.time() - t0, self.scores, self._no_speech_token,
            sorted(eos_id_set(getattr(model.generation_config, "eos_token_id", None))),
        )

    def transcribe(self, audio: np.ndarray, language: str, punctuation: bool) -> Transcript:
        proc, model = self._processor, self._model
        try:
            inputs = proc(
                audio=audio, sampling_rate=16000, return_tensors="pt",
                language=language, punctuation=punctuation,
            )
        except ValueError as e:
            # Scoped to this one call: an unknown language is by far the likeliest
            # ValueError from the processor, and a ValueError from anywhere later
            # is still the 500 it should be.
            raise BackendRequestError(f"the processor refused the request: {e}") from e
        audio_chunk_index = inputs.get("audio_chunk_index")
        inputs = inputs.to(model.device, dtype=model.dtype)
        # `return_dict_in_generate=True` changes what generate returns: every
        # reader below uses `.sequences`.
        gen = model.generate(
            **inputs,
            max_new_tokens=self.max_new_tokens,
            return_dict_in_generate=True,
            output_scores=self.scores,
        )
        decode_kwargs = dict(skip_special_tokens=True, language=language)
        if audio_chunk_index is not None:
            decode_kwargs["audio_chunk_index"] = audio_chunk_index
        decoded = proc.decode(gen.sequences, **decode_kwargs)
        text = decoded[0] if isinstance(decoded, list) else decoded
        return Transcript(
            text=text,
            finish_reason=finish_reason_of(
                gen.sequences.tolist(),
                eos_id_set(getattr(model.generation_config, "eos_token_id", None)),
            ),
            avg_logprob=mean_logprob(self._token_logprobs(gen)),
            no_speech_prob=self._no_speech_prob(gen),
        )

    def _token_logprobs(self, gen):
        """Per-token logprobs, or None. Every failure is None, never a 500: a
        transcript must not be lost to a telemetry field."""
        if not self.scores or not getattr(gen, "scores", None):
            return None
        try:
            scores = self._model.compute_transition_scores(gen.sequences, gen.scores, normalize_logits=True)
            return scores.flatten().tolist()
        except Exception as e:  # noqa: BLE001
            log.warning("could not compute transition scores: %s", e)
            return None

    def _no_speech_prob(self, gen):
        """P(no-speech token) over every generated step (chunk 0 only), or None."""
        if self._no_speech_token is None or not getattr(gen, "scores", None):
            return None
        try:
            import torch

            tid = int(self._no_speech_token)
            per_step = [torch.softmax(step[0].float(), dim=-1)[tid].item() for step in gen.scores]
            return no_speech_prob_of(per_step)
        except Exception as e:  # noqa: BLE001
            log.warning("could not read the no-speech probability: %s", e)
            return None
