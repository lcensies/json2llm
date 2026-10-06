#!/usr/bin/env bash
# Self-check for json2llm. Runs offline with --backend local; set
# J2L_BACKEND=openai|pi|claude|codex|opencode to also check that a real model
# compiles the sample manifest to the same op list (costs tokens, needs network).
set -u

cd "$(dirname "$0")"
go build -o json2llm . || exit 1
BIN=$PWD/json2llm
TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
fails=0

ok()   { printf 'ok   %s\n' "$1"; }
fail() { printf 'FAIL %s\n     %s\n' "$1" "${2:-}"; fails=$((fails + 1)); }

check() { # name expected actual
  if [ "$2" = "$3" ]; then ok "$1"; else fail "$1" "expected [$2], got [$3]"; fi
}

exits() { # name code cmd...
  local name=$1 want=$2; shift 2
  "$@" >/dev/null 2>&1; local got=$?
  check "$name" "$want" "$got"
}

# ---- plan (dry run) --------------------------------------------------------
# Preorder, document order — the plan is meant to diff against the input JSON.
want_plan=$(cat <<'EOF'
file   greeting
dir    dir
file   dir/subfile
dir    dir/subdir
link   symlink -> target path
exec   script
EOF
)
got_plan=$("$BIN" -b local -n -o "$TMP/nope" example-tree.json)
check "plan matches" "$want_plan" "$got_plan"
[ -e "$TMP/nope" ] && fail "dry run creates nothing" "$TMP/nope exists" || ok "dry run creates nothing"

# ---- wet run ---------------------------------------------------------------
"$BIN" -b local -o "$TMP/out" example-tree.json || fail "wet run" "nonzero exit"
check "file content"    "Hello, world!" "$(cat "$TMP/out/greeting")"
check "nested file"     "Content."      "$(cat "$TMP/out/dir/subfile")"
check "empty subdir"    ""              "$(ls -A "$TMP/out/dir/subdir")"
check "symlink target"  "target path"   "$(readlink "$TMP/out/symlink")"
check "script runs"     "Howdy!"        "$("$TMP/out/script")"
check "script mode"     "755"           "$(stat -c %a "$TMP/out/script")"
check "file mode"       "644"           "$(stat -c %a "$TMP/out/greeting")"

# Rerunning reaches the same state (replace, not append).
"$BIN" -b local -o "$TMP/out" example-tree.json || fail "rerun" "nonzero exit"
check "rerun idempotent" "Hello, world!" "$(cat "$TMP/out/greeting")"
check "rerun plan (replacing)" "file   greeting (replacing)" \
  "$("$BIN" -b local -n -o "$TMP/out" <<<'{"greeting":"x"}')"

# ---- symlink at an entry path is replaced, never followed ------------------
mkdir -p "$TMP/victim"; echo precious > "$TMP/victim/keep"
ln -s "$TMP/victim/keep" "$TMP/out/f"
echo '{"f":"safe"}' | "$BIN" -b local -o "$TMP/out" || fail "replace symlink" "nonzero exit"
check "symlink not followed" "precious" "$(cat "$TMP/victim/keep")"
check "entry replaced"       "safe"     "$(cat "$TMP/out/f")"
[ -L "$TMP/out/f" ] && fail "entry is a regular file" "still a symlink" || ok "entry is a regular file"

# A symlink standing where a directory belongs: replaced, not recursed into.
ln -s "$TMP/victim" "$TMP/out/d"
echo '{"d":{"x":"y"}}' | "$BIN" -b local -o "$TMP/out" || fail "replace dir symlink" "nonzero exit"
check "victim dir survives" "precious" "$(cat "$TMP/victim/keep")"
check "real dir created"    "y"        "$(cat "$TMP/out/d/x")"

# ---- the model's op list is not trusted ------------------------------------
cat > "$TMP/escape.json" <<'EOF'
{"ops":[{"kind":"file","path":"../escaped","content":"pwned","target":""}]}
EOF
exits "path escape refused" 4 "$BIN" -b "file:$TMP/escape.json" -o "$TMP/out" example-tree.json
[ -e "$TMP/escaped" ] && fail "nothing escaped --out" "$TMP/escaped exists" || ok "nothing escaped --out"

# Valid ops that disagree with the local compile: refused unless --trust-llm.
cat > "$TMP/wrong.json" <<'EOF'
{"ops":[{"kind":"file","path":"hallucinated","content":"Hello, world!","target":""}]}
EOF
exits "disagreement refused" 4 "$BIN" -b "file:$TMP/wrong.json" -o "$TMP/out" example-tree.json
[ -e "$TMP/out/hallucinated" ] && fail "refusal wrote nothing" "file was created" || ok "refusal wrote nothing"
"$BIN" -b "file:$TMP/wrong.json" --trust-llm -o "$TMP/out" example-tree.json || fail "--trust-llm applies" "nonzero exit"
check "--trust-llm applies" "Hello, world!" "$(cat "$TMP/out/hallucinated" 2>&1)"

# An escape still loses with --trust-llm: validation is not optional.
exits "--trust-llm still validates" 4 "$BIN" -b "file:$TMP/escape.json" --trust-llm -o "$TMP/out" example-tree.json

# ---- refusals --------------------------------------------------------------
exits "no-clobber refuses" 2 env sh -c \
  "echo '{\"greeting\":\"x\"}' | '$BIN' -b local -o '$TMP/out' --no-clobber"
for bad in '[]' '"str"' 'null' '{"n":1}' '{"b":true}' '{"a/b":"x"}' '{"":"x"}' \
           '{"..":"x"}' '{"a":["wat","x"]}' '{"a":["link"]}' '{"a":["link",1]}' \
           '{"a":["script","x","y"]}' '{oops' ; do
  got=$(printf '%s' "$bad" | "$BIN" -b local -n -o "$TMP/out" >/dev/null 2>&1; echo $?)
  [ "$got" = 2 ] && ok "rejects $bad" || fail "rejects $bad" "exit $got, wanted 2"
done
exits "unknown backend fails" 4 "$BIN" -b nope -n example-tree.json
exits "bad flag is a usage error" 1 "$BIN" --wat example-tree.json

# ---- drop-in compatibility with json2dir, when it is installed -------------
if command -v json2dir >/dev/null; then
  check "same plan as json2dir" \
    "$(json2dir -n -o "$TMP/zig" example-tree.json)" "$want_plan"
  json2dir -o "$TMP/zig" example-tree.json
  "$BIN" -b local -o "$TMP/mine" example-tree.json
  if diff -r --no-dereference "$TMP/zig" "$TMP/mine" >/dev/null; then
    ok "same tree as json2dir"
  else
    fail "same tree as json2dir" "$(diff -r --no-dereference "$TMP/zig" "$TMP/mine" | head -5)"
  fi
else
  printf 'skip json2dir comparison (not on PATH)\n'
fi

# ---- real model, opt-in ---------------------------------------------------
if [ -n "${J2L_BACKEND:-}" ]; then
  # No --trust-llm: a mismatch against the local compile is exit 4.
  if out=$("$BIN" -b "$J2L_BACKEND" -n -o "$TMP/nope2" example-tree.json 2>&1); then
    check "$J2L_BACKEND plan matches" "$want_plan" "$out"
  else
    fail "$J2L_BACKEND plan matches" "$out"
  fi
else
  printf 'skip LLM backends (set J2L_BACKEND to run one)\n'
fi

printf '\n%s\n' "$([ "$fails" = 0 ] && echo 'all checks passed' || echo "$fails check(s) failed")"
[ "$fails" = 0 ]
