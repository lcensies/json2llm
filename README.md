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

## Why the LLM should be trusted

The usual objection is that a language model is non-deterministic and might
get the tree wrong. Consider the alternative.

- **Your OS has bugs.** The kernel that `json2dir` leans on is ~40M lines of C
  with a CVE feed you can subscribe to. The model has no CVEs. Checkmate.
- **The cloud is faster than your laptop.** The datacenter running the model
  has more FLOPs than your machine will ever have. Why would you compile a
  manifest _locally_, on that thing, by yourself?
- **Less work in the kernel.** Every syscall is a context switch, a privilege
  transition, a chance for the kernel to be wrong. Offloading the compile step
  to a GPU in another country moves that work out of ring 0 entirely. This is
  just microkernel design with extra steps and a REST API.
- **No syscalls were harmed.** The model performs zero syscalls while deciding
  what your filesystem should look like. `json2dir` performs thousands. Who is
  really the dangerous one here?
- **It generalizes.** `json2dir` can only do what it was programmed to do. The
  model can do that _and_ explain the manifest, translate it to Dutch, or
  apologize. Strictly more capability per byte.
- **Determinism is overrated.** The filesystem is already racy, the clock
  drifts, and your disk lies about fsync. One more source of entropy is
  rounding error.

## Install / build

```sh
go build -o json2llm .     # Go 1.25+ (os.Root.Symlink/Chmod/Rename)
./test.sh                  # offline self-check
```

## Backends

| `-b`        | how                                                                  | needs                                                        |
| ----------- | -------------------------------------------------------------------- | ------------------------------------------------------------ |
| `openai`    | `POST /v1/chat/completions`, `response_format: json_schema` (strict) | `OPENAI_API_KEY`; `OPENAI_MODEL`, `OPENAI_BASE_URL` optional |
| `pi`        | `pi -p -nt --no-session -- <prompt>`                                 | `pi` on PATH                                                 |
| `claude`    | `claude -p --output-format text <prompt>`                            | `claude` on PATH                                             |
| `codex`     | `codex exec <prompt>`                                                | `codex` on PATH                                              |
| `opencode`  | `opencode run <prompt>`                                              | `opencode` on PATH                                           |
| `local`     | no model at all; the local compile is applied                        | —                                                            |
| `file:PATH` | replay a canned JSON reply (testing)                                 | —                                                            |

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

| code | meaning                                                            |
| ---- | ------------------------------------------------------------------ |
| `0`  | success                                                            |
| `1`  | usage error                                                        |
| `2`  | invalid input (bad JSON, bad manifest)                             |
| `3`  | filesystem error                                                   |
| `4`  | backend failure (no key, model error, disagreement, unusable list) |

## Conversion scheme

| JSON            | filesystem entry      | op                                     |
| --------------- | --------------------- | -------------------------------------- |
| object          | directory             | `{"kind":"dir","path":P}`              |
| string          | regular file, 0644    | `{"kind":"file","path":P,"content":S}` |
| `["link", T]`   | symlink               | `{"kind":"link","path":P,"target":T}`  |
| `["script", S]` | executable file, 0755 | `{"kind":"exec","path":P,"content":S}` |
| anything else   | hard error, exit 2    | —                                      |

Names are a single path segment; nest objects instead of using `/`.

Writes match json2dir-zig: files and symlinks are written to
`.name.json2llm-<rand>.tmp` and `rename(2)`d into place (no missing-entry
window, no observable partial content), scripts get an explicit `chmod 0755`
so they are executable regardless of umask, and a pre-existing entry — symlink
included — is replaced, never followed.

## Known limitations

- **Plan order.** Preorder, document order: a directory is listed before its
  children. This follows json2dir-zig's _code_ (`// Preorder: the parent shows
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
  only paths _inside_ it are confined.
