package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// opSchema is the strict JSON schema the model must answer in. OpenAI's
// structured outputs require every property listed in "required", so unused
// fields come back as empty strings rather than being omitted.
const opSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["ops"],
  "properties": {
    "ops": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["kind", "path", "content", "target"],
        "properties": {
          "kind": { "type": "string", "enum": ["dir", "file", "exec", "link"] },
          "path": { "type": "string" },
          "content": { "type": "string" },
          "target": { "type": "string" }
        }
      }
    }
  }
}`

const systemPrompt = `You compile a json2dir manifest into a flat list of filesystem operations.

Conversion scheme:
  object               -> {"kind":"dir",  "path":P}
  string S             -> {"kind":"file", "path":P, "content":S}
  ["script", S]        -> {"kind":"exec", "path":P, "content":S}
  ["link", T]          -> {"kind":"link", "path":P, "target":T}

Rules:
  - P is the slash-joined path from the document root; root keys have no prefix.
  - Preorder: emit a directory before its children.
  - Preserve the document's key order exactly.
  - Unused fields are the empty string.
  - Copy content and target byte for byte; never reformat, reflow or fix them.
  - Emit ops only. No commentary, no markdown.`

func ask(backend, model string, doc []byte) ([]Op, error) {
	switch backend {
	case "openai":
		return askOpenAI(model, doc)
	case "pi", "claude", "codex", "opencode":
		return askCLI(backend, doc)
	}
	// file:PATH replays a canned reply — for testing the parse/validate path
	// without spending tokens.
	if p, ok := strings.CutPrefix(backend, "file:"); ok {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		return parsePlan(string(b))
	}
	return nil, fmt.Errorf("unknown backend (want openai, pi, claude, codex, opencode or local)")
}

type plan struct {
	Ops []Op `json:"ops"`
}

func askOpenAI(model string, doc []byte) ([]Op, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, errors.New("OPENAI_API_KEY is not set (use --backend local to compile without an LLM)")
	}
	if model == "" {
		model = os.Getenv("OPENAI_MODEL")
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}

	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": string(doc)},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "json2dir_plan",
				"strict": true,
				"schema": json.RawMessage(opSchema),
			},
		},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Error struct{ Message string } `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("HTTP %d: unreadable response: %v", resp.StatusCode, err)
	}
	if out.Error.Message != "" {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("HTTP %d: no choices returned", resp.StatusCode)
	}
	return parsePlan(out.Choices[0].Message.Content)
}

// cliBackends are coding-agent CLIs in one-shot, no-tools mode. They have no
// structured-output knob, so the schema goes in the prompt and the answer gets
// scraped out of stdout.
var cliBackends = map[string][]string{
	"pi":       {"pi", "-p", "-nt", "--no-session", "--"},
	"claude":   {"claude", "-p", "--output-format", "text"},
	"codex":    {"codex", "exec"},
	"opencode": {"opencode", "run"},
}

func askCLI(backend string, doc []byte) ([]Op, error) {
	argv := cliBackends[backend]
	if _, err := exec.LookPath(argv[0]); err != nil {
		return nil, fmt.Errorf("%s is not on PATH", argv[0])
	}
	prompt := systemPrompt + "\n\nAnswer with one JSON object matching this schema:\n" +
		opSchema + "\n\nManifest:\n" + string(doc)

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(argv[0], append(argv[1:], prompt)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parsePlan(stdout.String())
}

// parsePlan pulls the JSON object out of a model reply that may be fenced or
// padded with prose.
func parsePlan(s string) ([]Op, error) {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return nil, fmt.Errorf("no JSON object in the reply: %.200q", s)
	}
	var p plan
	if err := json.Unmarshal([]byte(s[i:j+1]), &p); err != nil {
		return nil, fmt.Errorf("unparseable reply: %v", err)
	}
	if len(p.Ops) == 0 {
		return nil, errors.New("the reply contained no ops")
	}
	return p.Ops, nil
}
