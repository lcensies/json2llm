// json2llm materializes a JSON document as a directory tree — by asking an LLM
// to compile the document into a flat op list, then applying that list itself.
//
// Conversion scheme is byte-for-byte json2dir's:
//
//	object               -> directory (keys are entry names)
//	string               -> regular file (0644)
//	["link", target]     -> symlink
//	["script", contents] -> executable file (0755)
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path"
	"strings"
)

const version = "json2llm 0.1.0"

const usage = `Usage: json2llm [OPTIONS] [FILE]

Materialize a JSON document as a directory tree, via an LLM. Reads stdin by
default; with FILE, reads that file. The root of the document must be an
object; its keys become entries of the target directory.

Options:
  -o, --out <DIR>      Target directory (default: .). Created if missing.
  -b, --backend <B>    openai | pi | claude | codex | opencode | local
                       (default: openai; local compiles without an LLM)
  -m, --model <M>      Model for the openai backend (default: $OPENAI_MODEL
                       or gpt-4o-mini)
  -n, --dry-run        Validate and print the plan; write nothing.
  -v, --verbose        Print one line per entry while applying.
      --no-clobber     Fail instead of replacing existing entries.
      --trust-llm      Apply the model's op list even where it disagrees with
                       the locally compiled one (path validation still applies).
      --show-ops       Print the op list as JSON and exit.
  -h, --help           Print this help and exit.
  -V, --version        Print version and exit.

Exit codes:
  0  success
  1  usage error
  2  invalid input (bad JSON, bad manifest)
  3  filesystem error
  4  backend failure (no key, model error, model disagreed with local compile)
`

// Op is one filesystem action. It is both the LLM's output schema and what
// apply consumes.
type Op struct {
	Kind    string `json:"kind"` // dir | file | exec | link
	Path    string `json:"path"`
	Content string `json:"content"`
	Target  string `json:"target"`
}

type opts struct {
	out       string
	dryRun    bool
	verbose   bool
	noClobber bool
}

func die(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "json2llm: "+format+"\n", args...)
	os.Exit(code)
}

func main() {
	var (
		out      = "."
		backend  = "openai"
		model    = ""
		dryRun   bool
		verbose  bool
		noClob   bool
		trustLLM bool
		showOps  bool
		showVer  bool
	)
	fs := flag.NewFlagSet("json2llm", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, s := range []struct {
		long, short string
		p           *string
		def         string
	}{
		{"out", "o", &out, "."},
		{"backend", "b", &backend, "openai"},
		{"model", "m", &model, ""},
	} {
		fs.StringVar(s.p, s.long, s.def, "")
		fs.StringVar(s.p, s.short, s.def, "")
	}
	for _, s := range []struct {
		long, short string
		p           *bool
	}{
		{"dry-run", "n", &dryRun},
		{"verbose", "v", &verbose},
		{"no-clobber", "", &noClob},
		{"trust-llm", "", &trustLLM},
		{"show-ops", "", &showOps},
		{"version", "V", &showVer},
	} {
		fs.BoolVar(s.p, s.long, false, "")
		if s.short != "" {
			fs.BoolVar(s.p, s.short, false, "")
		}
	}

	// Parse flags that appear after the positional too, which flag stops at.
	var input string
	args := os.Args[1:]
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Print(usage)
				os.Exit(0)
			}
			fmt.Fprintf(os.Stderr, "json2llm: %v\n\n%s", err, usage)
			os.Exit(1)
		}
		if fs.NArg() == 0 {
			break
		}
		if input != "" {
			fmt.Fprintf(os.Stderr, "json2llm: unexpected extra argument %q\n\n%s", fs.Arg(0), usage)
			os.Exit(1)
		}
		input, args = fs.Arg(0), fs.Args()[1:]
	}
	if showVer {
		fmt.Println(version)
		return
	}

	data, err := readInput(input)
	if err != nil {
		die(3, "%v", err)
	}

	// Local compile is the reference: the plan the LLM is supposed to produce.
	local, err := compile(data)
	if err != nil {
		die(2, "%v", err)
	}

	ops := local
	if backend != "local" {
		llm, err := ask(backend, model, data)
		if err != nil {
			die(4, "%s backend: %v", backend, err)
		}
		if err := validateOps(llm); err != nil {
			die(4, "%s backend returned an unusable op list: %v", backend, err)
		}
		if d := diff(local, llm); d != "" && !trustLLM {
			die(4, "%s backend disagreed with the local compile: %s\n"+
				"       (pass --trust-llm to apply the model's list anyway)", backend, d)
		}
		ops = llm
	}

	if showOps {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(ops); err != nil {
			die(3, "%v", err)
		}
		return
	}

	if err := apply(ops, opts{out: out, dryRun: dryRun, verbose: verbose, noClobber: noClob}); err != nil {
		var me manifestError
		if errors.As(err, &me) {
			die(2, "%v", err)
		}
		die(3, "%v", err)
	}
}

func readInput(name string) ([]byte, error) {
	if name == "" || name == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(name)
}

// ---- local compile ---------------------------------------------------------

type manifestError struct{ error }

func manifestf(format string, args ...any) error {
	return manifestError{fmt.Errorf(format, args...)}
}

// compile walks the document with a token decoder rather than unmarshalling
// into a map, because object key order is part of the output contract.
func compile(data []byte) ([]Op, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	t, err := d.Token()
	if err != nil {
		return nil, manifestf("invalid JSON: %v", err)
	}
	if t != json.Delim('{') {
		return nil, manifestf("<root>: the document root must be an object")
	}
	var ops []Op
	if err := walkObject(d, "", &ops); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, manifestf("trailing data after the document")
	}
	return ops, nil
}

func walkObject(d *json.Decoder, prefix string, ops *[]Op) error {
	for {
		t, err := d.Token()
		if err != nil {
			return manifestf("invalid JSON: %v", err)
		}
		if t == json.Delim('}') {
			return nil
		}
		name, ok := t.(string)
		if !ok {
			return manifestf("invalid JSON: expected an object key")
		}
		if err := validateName(name); err != nil {
			return manifestf("%s: %v", join(prefix, name), err)
		}
		if err := walkValue(d, join(prefix, name), ops); err != nil {
			return err
		}
	}
}

func walkValue(d *json.Decoder, p string, ops *[]Op) error {
	t, err := d.Token()
	if err != nil {
		return manifestf("invalid JSON: %v", err)
	}
	switch v := t.(type) {
	case string:
		*ops = append(*ops, Op{Kind: "file", Path: p, Content: v})
		return nil
	case json.Delim:
		switch v {
		case '{':
			// Preorder: the parent is listed before its children, so applying
			// the list top to bottom creates directories before their contents.
			*ops = append(*ops, Op{Kind: "dir", Path: p})
			return walkObject(d, p, ops)
		case '[':
			return walkArray(d, p, ops)
		}
	}
	return manifestf("%s: unsupported value type; use an object, a string, "+
		`["link", ...] or ["script", ...]`, p)
}

func walkArray(d *json.Decoder, p string, ops *[]Op) error {
	bad := manifestf(`%s: arrays must be ["link", target] or ["script", contents]`, p)
	var items []string
	for {
		t, err := d.Token()
		if err != nil {
			return manifestf("invalid JSON: %v", err)
		}
		if t == json.Delim(']') {
			break
		}
		s, ok := t.(string)
		if !ok {
			return bad
		}
		items = append(items, s)
	}
	if len(items) != 2 {
		return bad
	}
	switch items[0] {
	case "link":
		*ops = append(*ops, Op{Kind: "link", Path: p, Target: items[1]})
	case "script":
		*ops = append(*ops, Op{Kind: "exec", Path: p, Content: items[1]})
	default:
		return manifestf("%s: unknown array tag %q (expected \"link\" or \"script\")", p, items[0])
	}
	return nil
}

func validateName(name string) error {
	switch {
	case name == "":
		return errors.New("empty entry name")
	case name == "." || name == "..":
		return errors.New(`"." and ".." are not valid entry names`)
	case strings.Contains(name, "/"):
		return errors.New("path separators are not allowed in entry names; nest objects instead")
	case strings.ContainsRune(name, 0):
		return errors.New("NUL byte in entry name")
	}
	return nil
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// validateOps re-checks a model-supplied op list. This is the trust boundary:
// everything below it writes to the filesystem.
func validateOps(ops []Op) error {
	for i, op := range ops {
		switch op.Kind {
		case "dir", "file", "exec", "link":
		default:
			return fmt.Errorf("op %d: unknown kind %q", i, op.Kind)
		}
		if op.Path == "" {
			return fmt.Errorf("op %d: empty path", i)
		}
		for _, seg := range strings.Split(op.Path, "/") {
			if err := validateName(seg); err != nil {
				return fmt.Errorf("op %d (%q): %v", i, op.Path, err)
			}
		}
	}
	return nil
}

func diff(want, got []Op) string {
	if len(want) != len(got) {
		return fmt.Sprintf("%d ops expected, %d returned", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Sprintf("op %d: expected %v, got %v", i, want[i], got[i])
		}
	}
	return ""
}

// ---- apply -----------------------------------------------------------------

func apply(ops []Op, o opts) error {
	// A nil root means "the target directory does not exist" — only reachable
	// on a dry run, where nothing exists yet and nothing gets created.
	root, err := os.OpenRoot(o.out)
	if err != nil {
		if !o.dryRun {
			if err := os.MkdirAll(o.out, 0o755); err != nil {
				return fmt.Errorf("cannot create target directory %q: %v", o.out, err)
			}
			if root, err = os.OpenRoot(o.out); err != nil {
				return fmt.Errorf("cannot open target directory %q: %v", o.out, err)
			}
		} else {
			root = nil
		}
	}
	if root != nil {
		defer root.Close()
	}

	for _, op := range ops {
		existed, wasDir := false, false
		if root != nil {
			if st, err := root.Lstat(op.Path); err == nil {
				existed, wasDir = true, st.IsDir()
			}
		}
		if existed && o.noClobber {
			return manifestf("%s: entry already exists and --no-clobber is set", op.Path)
		}
		if !o.dryRun {
			if err := place(root, op, existed, wasDir); err != nil {
				return fmt.Errorf("%s: %v", op.Path, err)
			}
		}
		if o.dryRun || o.verbose {
			fmt.Println(planLine(op, existed))
		}
	}
	return nil
}

func place(root *os.Root, op Op, existed, wasDir bool) error {
	if op.Kind == "dir" {
		if existed {
			// RemoveAll does not follow symlinks: a symlink here is unlinked,
			// its target left alone.
			if err := root.RemoveAll(op.Path); err != nil {
				return fmt.Errorf("cannot remove existing entry: %v", err)
			}
		}
		if err := root.Mkdir(op.Path, 0o755); err != nil {
			return fmt.Errorf("mkdir: %v", err)
		}
		return nil
	}
	// Files and symlinks land via rename(2): no window where the entry is
	// missing, no partially written content ever observable.
	if existed && wasDir {
		if err := root.RemoveAll(op.Path); err != nil {
			return fmt.Errorf("cannot remove existing directory: %v", err)
		}
	}
	tmp := path.Join(path.Dir(op.Path), fmt.Sprintf(".%s.json2llm-%x.tmp", path.Base(op.Path), rand.Uint32()))
	if op.Kind == "link" {
		if err := root.Symlink(op.Target, tmp); err != nil {
			return fmt.Errorf("cannot create symlink: %v", err)
		}
	} else {
		mode := os.FileMode(0o644)
		if op.Kind == "exec" {
			mode = 0o755
		}
		f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return fmt.Errorf("cannot create file: %v", err)
		}
		if _, err := f.WriteString(op.Content); err != nil {
			f.Close()
			return fmt.Errorf("write failed: %v", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close failed: %v", err)
		}
		// Explicit chmod: O_CREATE's mode is masked by umask, and scripts must
		// be 0755 regardless of the caller's.
		if err := root.Chmod(tmp, mode); err != nil {
			return fmt.Errorf("chmod failed: %v", err)
		}
	}
	if err := root.Rename(tmp, op.Path); err != nil {
		root.Remove(tmp)
		return fmt.Errorf("cannot move entry into place: %v", err)
	}
	return nil
}

func planLine(op Op, replacing bool) string {
	tag := ""
	if replacing {
		tag = " (replacing)"
	}
	if op.Kind == "link" {
		return fmt.Sprintf("%-6s %s -> %s%s", op.Kind, op.Path, op.Target, tag)
	}
	return fmt.Sprintf("%-6s %s%s", op.Kind, op.Path, tag)
}
