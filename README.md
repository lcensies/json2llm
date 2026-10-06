# json2llm

> JSON documents → directory trees, **via an LLM**. Same conversion scheme as
> [json2dir-zig](https://github.com/71g3pf4c3/json2dir-zig) and
> [alurm/json2dir](https://github.com/alurm/json2dir) — same JSON in, same tree
> out, except a language model does the compiling.

The model is handed the manifest and asked to compile it into a flat op list
under a strict JSON schema. Go validates that list and performs the writes.

```
$ json2llm -b pi -n example-tree.json
file   greeting
dir    dir
file   dir/subfile
dir    dir/subdir
link   symlink -> target path
exec   script

$ json2llm -b pi -o out example-tree.json
$ ./out/script
Howdy!
```

One static binary, zero dependencies (stdlib only — `net/http` and
`encoding/json` are the whole "SDK").

## Why the LLM is not trusted

The model's output is untrusted input, so it is never the authority:

1. The manifest is compiled **locally** first — that plan is the reference.
2. The model's op list is validated: known kinds only, and every path segment
   must pass the same name rules as the manifest (`""`, `.`, `..`, `/`, NUL
   rejected).
3. The two lists are compared. A mismatch is exit 4, before anything is
   written. `--trust-llm` applies the model's list anyway — validation still
   runs, so a hallucinated `../../etc/passwd` is still refused.
4. All writes go through `os.Root` (Go 1.25+), which resolves paths inside
   `--out` and refuses to traverse a symlink out of it — kernel-enforced via
   `openat2(RESOLVE_BENEATH)` where available.

So the LLM can be wrong, slow or adversarial; the worst it gets is exit 4.

Which leaves the honest summary: **the model adds nothing.** Step 1 already
produced the correct answer. This is json2dir with an expensive, non-
deterministic no-op bolted into the middle, and the safety machinery exists to
contain the no-op. Use `-b local` and it is simply json2dir.

## Install / build

```sh
go build -o json2llm .     # Go 1.25+ (os.Root.Symlink/Chmod/Rename)
./test.sh                  # offline self-check
```

## Backends

| `-b` | how | needs |
| --- | --- | --- |
| `openai` | `POST /v1/chat/completions`, `response_format: json_schema` (strict) | `OPENAI_API_KEY`; `OPENAI_MODEL`, `OPENAI_BASE_URL` optional |
| `pi` | `pi -p -nt --no-session -- <prompt>` | `pi` on PATH |
| `claude` | `claude -p --output-format text <prompt>` | `claude` on PATH |
| `codex` | `codex exec <prompt>` | `codex` on PATH |
| `opencode` | `opencode run <prompt>` | `opencode` on PATH |
| `local` | no model at all; the local compile is applied | — |
| `file:PATH` | replay a canned JSON reply (testing) | — |

Default is `openai`. The CLI backends have no structured-output knob, so the
schema goes in the prompt and the reply is scraped for its JSON object.

Verified end to end here with `-b pi`: the model returns the op list
byte-identically to the local compile, so the plans match and the run proceeds.

## Usage

```
Usage: json2llm [OPTIONS] [FILE]

  -o, --out <DIR>      Target directory (default: .). Created if missing.
  -b, --backend <B>    openai | pi | claude | codex | opencode | local
  -m, --model <M>      Model for the openai backend
  -n, --dry-run        Validate and print the plan; write nothing.
  -v, --verbose        Print one line per entry while applying.
      --no-clobber     Fail instead of replacing existing entries.
      --trust-llm      Apply the model's list even where it disagrees locally.
      --show-ops       Print the op list as JSON and exit.
  -h, --help           Print this help and exit.
  -V, --version        Print version and exit.
```

| code | meaning |
| --- | --- |
| `0` | success |
| `1` | usage error |
| `2` | invalid input (bad JSON, bad manifest) |
| `3` | filesystem error |
| `4` | backend failure (no key, model error, disagreement, unusable list) |

## Conversion scheme

| JSON | filesystem entry | op |
| --- | --- | --- |
| object | directory | `{"kind":"dir","path":P}` |
| string | regular file, 0644 | `{"kind":"file","path":P,"content":S}` |
| `["link", T]` | symlink | `{"kind":"link","path":P,"target":T}` |
| `["script", S]` | executable file, 0755 | `{"kind":"exec","path":P,"content":S}` |
| anything else | hard error, exit 2 | — |

Names are a single path segment; nest objects instead of using `/`.

Writes match json2dir-zig: files and symlinks are written to
`.name.json2llm-<rand>.tmp` and `rename(2)`d into place (no missing-entry
window, no observable partial content), scripts get an explicit `chmod 0755`
so they are executable regardless of umask, and a pre-existing entry — symlink
included — is replaced, never followed.

## Known limitations

- **Plan order.** Preorder, document order: a directory is listed before its
  children. This follows json2dir-zig's *code* (`// Preorder: the parent shows
  up before its children`); its README's example output shows children first
  and is stale. `./test.sh` diffs against `json2dir` automatically when it is
  on PATH.
- **Non-determinism is the feature.** Two runs can produce different op lists.
  Without `--trust-llm` a wrong one is caught and refused; with it, you get
  whatever the model said (within path validation).
- **Big manifests.** The whole document goes in one prompt and the whole op
  list comes back in one reply. Past a few hundred KB you hit context limits —
  and content is echoed back verbatim, so cost scales with tree size. No
  chunking.
- **UTF-8 only.** JSON strings cannot carry arbitrary bytes. Same in any
  implementation of this scheme.
- **Unix only.** Symlinks, modes and `rename(2)` semantics.
- **Directory replacement is not atomic.** A dir entry is `RemoveAll`ed then
  `Mkdir`ed, as upstream does; a crash in that window leaves it missing.
- **`os.Root` costs a syscall per path component** on kernels without
  `openat2`. Irrelevant next to a model round-trip.
- **No `--out` sandbox for the manifest itself.** `-o` is resolved normally;
  only paths *inside* it are confined.

## Prior art in this repo's history

Asked for Brainfuck first. Brainfuck has no syscalls — only stdin/stdout bytes
— so the honest version emits a shell script for `sh` to run. eBPF was also
floated: it cannot create files either (no `mkdirat`/`openat` helpers, verifier
forbids unbounded recursion), so it can only watch someone else do it. A Rust
kernel module with an ioctl would work and would put a JSON parser in ring 0.
Go it is.
