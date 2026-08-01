UNVERIFIED — lab output; verify against source before acting

# Model licence check — Qwen3-1.7B Q4_K_M GGUF

Date: 2026-07-31, resolved 2026-08-01 · Checked during Task 6 (build box) · Gate: spec §2
"verify the exact model file's licence before it ships on the public repo"

## Verdict

| Question | Answer |
|---|---|
| File shipped | `Qwen3-1.7B-Q4_K_M.gguf` → `models/qwen3-1.7b-q4.gguf` |
| Source repo | **`ggml-org/Qwen3-1.7B-GGUF`** |
| Licence | **Apache-2.0 — PASS** |
| Base model | `Qwen/Qwen3-1.7B` (Apache-2.0, independently verified below) |
| Gated / disabled | No / no |
| Downloaded + verified | **Yes** — sha256 `d2387ca2…c7b5`, 1 282 439 264 B |

## Sources checked

| URL | What it says |
|---|---|
| `https://huggingface.co/api/models/ggml-org/Qwen3-1.7B-GGUF` | `cardData.license` = `"apache-2.0"`; tag `license:apache-2.0`; `author` = `ggml-org`; `base_model` = `Qwen/Qwen3-1.7B`; `gated: false`, `disabled: false` |
| `https://huggingface.co/api/models/ggml-org/Qwen3-1.7B-GGUF/tree/main?recursive=true` | 5 entries: `.gitattributes`, `README.md`, and three GGUFs — `Q4_K_M` (1 282 439 264 B), `Q8_0`, `f16` |
| `https://huggingface.co/Qwen/Qwen3-1.7B-GGUF` + its API | the **official Qwen** GGUF repo — model-card label `License: apache-2.0`, ships a top-level `LICENSE` (11 544 B). Confirms the upstream weights are Apache-2.0. |

Apache-2.0 permits redistribution inside the skiff bundle, including on a public repo,
provided the licence text and attribution ride along.

## Why `ggml-org` and not the official Qwen repo

The plan called for **Qwen3-1.7B Q4_K_M from the official Qwen repo. That file does not
exist.** `Qwen/Qwen3-1.7B-GGUF` publishes exactly one GGUF — `Qwen3-1.7B-Q8_0.gguf`,
1.83 GB — and no Q4 at any size. Its complete contents are `.gitattributes`, `LICENSE`,
`Qwen3-1.7B-Q8_0.gguf`, `README.md`, `params`. A search of the Qwen org
(`/api/models?author=Qwen&search=Qwen3-1.7B`) returns no second GGUF repo; the other 1.7B
entries are safetensors base/instruct, FP8, GPTQ-Int8, and four MLX repos.

So a Q4_K_M has to come from a requantizer. Candidates, all Apache-2.0 where declared:

| Repo | Q4_K_M size | Note |
|---|---|---|
| **`ggml-org/Qwen3-1.7B-GGUF`** | **1 282 439 264 B (1.19 GiB)** | **chosen** |
| `unsloth/Qwen3-1.7B-GGUF` | 1 107 409 472 B | ~175 MB smaller; 26 quants published; most-downloaded |
| `bartowski/Qwen_Qwen3-1.7B-GGUF` | — | no `license` field declared in card metadata |
| `lmstudio-community/Qwen3-1.7B-GGUF` | — | no `license` field declared in card metadata |

**`ggml-org` was chosen because it is the llama.cpp organisation itself** — the exact
upstream `LLAMA_CPP_COMMIT` is already pinned from (`github.com/ggml-org/llama.cpp`). The
bundle therefore adds **no new trust anchor**: the same org supplies both the inference
binary and the weights conversion. That directly answers the objection to using a
third-party requant. Its three-file, hand-curated repo is also easier to reason about than
an automated 26-quant sweep.

`unsloth` is the reasonable alternative if the extra ~175 MB matters more than collapsing
the trust set — it is Apache-2.0 and by far the most downloaded of the candidates.

## Integrity

The download was verified twice over:

```
$ sha256sum models/qwen3-1.7b-q4.gguf
d2387ca2dbfee2ffabce7120d3770dadca0b293052bc2f0e138fdc940d9bc7b5

HuggingFace x-linked-etag: "d2387ca2dbfee2ffabce7120d3770dadca0b293052bc2f0e138fdc940d9bc7b5"
HuggingFace x-linked-size: 1282439264   (matches bytes on disk)
```

The locally computed sha matches the checksum HuggingFace records for the object, so the
pin in `build/pins.env` is independently corroborated rather than self-asserted.

## Outstanding obligation

**Apache-2.0 §4(a):** whichever task assembles the public/shipping bundle must include the
licence text alongside the GGUF. Note that `ggml-org/Qwen3-1.7B-GGUF` does **not** carry a
`LICENSE` file of its own — it declares Apache-2.0 in card metadata only. Take the licence
text from the base model `Qwen/Qwen3-1.7B` (or `Qwen/Qwen3-1.7B-GGUF`, which does ship a
`LICENSE`) and attribute both Qwen (weights) and ggml-org (quantization).
