package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const sample = `{
  "greeting": "Hello, world!",
  "dir": { "subfile": "Content.\n", "subdir": {} },
  "symlink": ["link", "target path"],
  "script":  ["script", "#!/bin/sh\necho Howdy!"]
}`

var sampleOps = []Op{
	{Kind: "file", Path: "greeting", Content: "Hello, world!"},
	{Kind: "dir", Path: "dir"},
	{Kind: "file", Path: "dir/subfile", Content: "Content.\n"},
	{Kind: "dir", Path: "dir/subdir"},
	{Kind: "link", Path: "symlink", Target: "target path"},
	{Kind: "exec", Path: "script", Content: "#!/bin/sh\necho Howdy!"},
}

// The conversion scheme, including preorder and document key order.
func TestCompile(t *testing.T) {
	got, err := compile([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, sampleOps) {
		t.Errorf("compile:\n got %v\nwant %v", got, sampleOps)
	}
}

// Key order is load-bearing (the plan is meant to diff against the input), and
// a map-based decode would lose it.
func TestCompilePreservesKeyOrder(t *testing.T) {
	got, err := compile([]byte(`{"z":"1","a":"2","m":"3"}`))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, op := range got {
		paths = append(paths, op.Path)
	}
	if want := []string{"z", "a", "m"}; !slices.Equal(paths, want) {
		t.Errorf("got %v, want %v", paths, want)
	}
}

func TestCompileRejects(t *testing.T) {
	for _, doc := range []string{
		`[]`, `"just a string"`, `null`, `{"n": 1}`, `{"b": true}`,
		`{"a/b": "x"}`, `{"": "x"}`, `{".": "x"}`, `{"..": "x"}`,
		`{"a": ["wat", "x"]}`, `{"a": ["link"]}`, `{"a": ["link", 1]}`,
		`{"a": ["script", "x", "y"]}`, `{"a": {"b": 1}}`, `{oops`, `{} {}`,
	} {
		if ops, err := compile([]byte(doc)); err == nil {
			t.Errorf("compile(%s) = %v, want an error", doc, ops)
		}
	}
}

func TestCompileAcceptsOddButLegalNames(t *testing.T) {
	for _, name := range []string{"...", ".hidden", "a b", "-", "dot.json"} {
		if _, err := compile([]byte(`{"` + name + `":"x"}`)); err != nil {
			t.Errorf("compile(%q) failed: %v", name, err)
		}
	}
}

// The trust boundary: anything the model returns is re-checked here.
func TestValidateOps(t *testing.T) {
	for _, op := range []Op{
		{Kind: "file", Path: "../escaped"},
		{Kind: "file", Path: "a/../../escaped"},
		{Kind: "file", Path: "/etc/passwd"},
		{Kind: "file", Path: ""},
		{Kind: "file", Path: "a/"},
		{Kind: "file", Path: "a/\x00b"},
		{Kind: "chmod", Path: "a"},
	} {
		if err := validateOps([]Op{op}); err == nil {
			t.Errorf("validateOps(%v) = nil, want an error", op)
		}
	}
	if err := validateOps(sampleOps); err != nil {
		t.Errorf("validateOps(sampleOps) = %v, want nil", err)
	}
}

func TestDiff(t *testing.T) {
	if d := diff(sampleOps, slices.Clone(sampleOps)); d != "" {
		t.Errorf("identical lists differ: %s", d)
	}
	short := sampleOps[:2]
	if diff(sampleOps, short) == "" {
		t.Error("length mismatch not reported")
	}
	tampered := slices.Clone(sampleOps)
	tampered[0].Content = "Goodbye"
	if diff(sampleOps, tampered) == "" {
		t.Error("content mismatch not reported")
	}
}

// Replies from CLI backends arrive fenced or padded with prose.
func TestParsePlan(t *testing.T) {
	want := []Op{{Kind: "file", Path: "a", Content: "b"}}
	for _, reply := range []string{
		`{"ops":[{"kind":"file","path":"a","content":"b","target":""}]}`,
		"Here you go:\n```json\n{\"ops\":[{\"kind\":\"file\",\"path\":\"a\",\"content\":\"b\"}]}\n```\nHope that helps!",
	} {
		got, err := parsePlan(reply)
		if err != nil {
			t.Fatalf("%q: %v", reply, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%q: got %v, want %v", reply, got, want)
		}
	}
	for _, reply := range []string{"", "I'm sorry Dave", `{"ops":[]}`, `{"ops": "nope"}`} {
		if _, err := parsePlan(reply); err == nil {
			t.Errorf("parsePlan(%q) = nil error, want one", reply)
		}
	}
}

// The whole backend path minus the network: a canned reply is parsed,
// validated and compared against the local compile.
func TestAskFileBackend(t *testing.T) {
	reply := filepath.Join(t.TempDir(), "reply.json")
	body := `{"ops":[
		{"kind":"file","path":"greeting","content":"Hello, world!","target":""},
		{"kind":"dir","path":"dir","content":"","target":""},
		{"kind":"file","path":"dir/subfile","content":"Content.\n","target":""},
		{"kind":"dir","path":"dir/subdir","content":"","target":""},
		{"kind":"link","path":"symlink","content":"","target":"target path"},
		{"kind":"exec","path":"script","content":"#!/bin/sh\necho Howdy!","target":""}]}`
	if err := os.WriteFile(reply, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ask("file:"+reply, "", []byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOps(got); err != nil {
		t.Fatal(err)
	}
	if d := diff(sampleOps, got); d != "" {
		t.Errorf("a faithful reply was rejected: %s", d)
	}

	if _, err := ask("telepathy", "", []byte(sample)); err == nil {
		t.Error("unknown backend accepted")
	}
}

func TestPlanLine(t *testing.T) {
	for _, c := range []struct {
		op        Op
		replacing bool
		want      string
	}{
		{Op{Kind: "file", Path: "greeting"}, false, "file   greeting"},
		{Op{Kind: "dir", Path: "dir"}, false, "dir    dir"},
		{Op{Kind: "exec", Path: "script"}, false, "exec   script"},
		{Op{Kind: "link", Path: "symlink", Target: "target path"}, false, "link   symlink -> target path"},
		{Op{Kind: "file", Path: "greeting"}, true, "file   greeting (replacing)"},
	} {
		if got := planLine(c.op, c.replacing); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestApply(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	if err := apply(sampleOps, opts{out: out}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, content string }{
		{"greeting", "Hello, world!"},
		{"dir/subfile", "Content.\n"},
		{"script", "#!/bin/sh\necho Howdy!"},
	} {
		b, err := os.ReadFile(filepath.Join(out, c.path))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.content {
			t.Errorf("%s = %q, want %q", c.path, b, c.content)
		}
	}
	if target, err := os.Readlink(filepath.Join(out, "symlink")); err != nil || target != "target path" {
		t.Errorf("symlink = %q, %v", target, err)
	}
	for _, c := range []struct {
		path string
		mode os.FileMode
	}{{"greeting", 0o644}, {"script", 0o755}} {
		st, err := os.Lstat(filepath.Join(out, c.path))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != c.mode {
			t.Errorf("%s mode = %o, want %o", c.path, st.Mode().Perm(), c.mode)
		}
	}
	// Rerunning converges rather than accumulating.
	if err := apply(sampleOps, opts{out: out}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDryRunWritesNothing(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	if err := apply(sampleOps, opts{out: out, dryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Errorf("dry run created %s", out)
	}
}

// A symlink planted at an entry path must be replaced, not followed.
func TestApplyReplacesSymlinkWithoutFollowing(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	victim := filepath.Join(tmp, "victim")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(out, "f")); err != nil {
		t.Fatal(err)
	}

	if err := apply([]Op{{Kind: "file", Path: "f", Content: "safe"}}, opts{out: out}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "precious" {
		t.Errorf("victim clobbered: %q", b)
	}
	st, err := os.Lstat(filepath.Join(out, "f"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		t.Error("entry is still a symlink")
	}
	if b, _ := os.ReadFile(filepath.Join(out, "f")); string(b) != "safe" {
		t.Errorf("entry = %q, want %q", b, "safe")
	}
}

// os.Root confines writes to --out even if validateOps were bypassed.
func TestApplyCannotEscapeOut(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "out")
	err := apply([]Op{{Kind: "file", Path: "../escaped", Content: "pwned"}}, opts{out: out})
	if err == nil {
		t.Fatal("apply escaped --out without error")
	}
	if _, err := os.Lstat(filepath.Join(tmp, "escaped")); !os.IsNotExist(err) {
		t.Error("a file was written outside --out")
	}
}

func TestApplyNoClobber(t *testing.T) {
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "greeting"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := apply(sampleOps, opts{out: out, noClobber: true})
	if err == nil {
		t.Fatal("no-clobber did not refuse")
	}
	if !strings.Contains(err.Error(), "--no-clobber") {
		t.Errorf("unhelpful error: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "greeting")); string(b) != "mine" {
		t.Errorf("refusal still wrote: %q", b)
	}
}
