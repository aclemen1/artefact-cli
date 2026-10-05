package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// Candidate is one possible answer: a child node.
type Candidate struct {
	Key, Label, Scope string
}

// Question asks which child of a node an artefact belongs to.
type Question struct {
	Plan       string
	Path       string // labels of the node chosen so far, for context
	Facts      Facts
	Candidates []Candidate
}

// Answer is a decider's choice: a candidate key, or "none".
type Answer struct {
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type Decider interface {
	Name() string
	Decide(ctx context.Context, q Question) (Answer, error)
}

const noneKey = "none"

func instructions(q Question) string {
	s := "Classify this file in the plan \"" + q.Plan + "\""
	if q.Path != "" {
		s += ", under \"" + q.Path + "\""
	}
	return s + ". Pick the class it belongs to, from what the file is and what it is for, or none if no class fits."
}

func state(q Question) string {
	var b strings.Builder
	fmt.Fprintf(&b, "File name: %s\nTitle: %s\nType: %s\nDate: %s\n", q.Facts.Filename, q.Facts.Title, q.Facts.Mime, q.Facts.Date)
	if q.Facts.Card != "" {
		b.WriteString("\nCard:\n" + q.Facts.Card + "\n")
	}
	if q.Facts.Text != "" {
		b.WriteString("\nContent (excerpt):\n" + q.Facts.Text + "\n")
	}
	return b.String()
}

// OpenJev asks a /v1/systemone server (OpenJev, or Jev with a key).
type OpenJev struct {
	Label string // decider name in the records; default openjev
	URL   string // e.g. http://127.0.0.1:3000
	Key   string // bearer token, for the hosted Jev
	Model string
	HTTP  *http.Client
}

func (o OpenJev) Name() string {
	if o.Label != "" {
		return o.Label
	}
	return "openjev"
}

func (o OpenJev) Decide(ctx context.Context, q Question) (Answer, error) {
	criteria := map[string]string{}
	for _, c := range q.Candidates {
		d := c.Label
		if c.Scope != "" {
			d += ": " + c.Scope
		}
		criteria[c.Key] = d
	}
	criteria[noneKey] = "None of these classes fits the file."
	model := o.Model
	if model == "" {
		model = "openjev"
	}
	body, _ := json.Marshal(map[string]any{
		"state": state(q), "model": model,
		"questions": map[string]any{"node": map[string]any{"type": "choice", "instructions": instructions(q), "criteria": criteria}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.URL, "/")+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return Answer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.Key != "" {
		req.Header.Set("Authorization", "Bearer "+o.Key)
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Answer{}, fmt.Errorf("%s at %s: %w (is the server reachable?)", o.Name(), o.URL, err)
	}
	defer resp.Body.Close()
	var out struct {
		Answers map[string]Answer `json:"answers"`
		Error   *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Answer{}, fmt.Errorf("openjev: unreadable answer (HTTP %d): %w", resp.StatusCode, err)
	}
	if out.Error != nil {
		return Answer{}, fmt.Errorf("%s: %s", o.Name(), out.Error.Message)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return Answer{}, fmt.Errorf("%s: HTTP %d", o.Name(), resp.StatusCode)
	}
	a, ok := out.Answers["node"]
	if !ok {
		return Answer{}, fmt.Errorf("openjev: no answer (HTTP %d)", resp.StatusCode)
	}
	return a, nil
}

// ACP asks an agent over the Agent Client Protocol, which may open the file.
type ACP struct {
	Command []string
	Timeout time.Duration
}

func (a ACP) Name() string { return "acp" }

var jsonRe = regexp.MustCompile(`(?s)\{[^{}]*"choice"[^{}]*\}`)

func (a ACP) Decide(ctx context.Context, q Question) (Answer, error) {
	if len(a.Command) == 0 {
		return Answer{}, errors.New("acp decider: no command (classify.deciders.acp.command in the configuration)")
	}
	var b strings.Builder
	b.WriteString(instructions(q) + "\n\n" + state(q))
	if q.Facts.File != "" {
		b.WriteString("\nThe file itself is at " + q.Facts.File + "; you may read it. Change nothing.\n")
	}
	b.WriteString("\nClasses:\n")
	for _, c := range q.Candidates {
		fmt.Fprintf(&b, "- %s: %s", c.Key, c.Label)
		if c.Scope != "" {
			b.WriteString(" — " + c.Scope)
		}
		b.WriteString("\n")
	}
	b.WriteString("- none: none of these classes fits.\n\nAnswer with one line of JSON and nothing else: {\"choice\": \"<key>\", \"confidence\": <0 to 1>}\n")
	text, err := runACP(ctx, a.Command, b.String(), a.Timeout)
	if err != nil {
		return Answer{}, err
	}
	m := jsonRe.FindString(text)
	if m == "" {
		return Answer{}, fmt.Errorf("acp decider: no JSON answer in %q", truncate(text, 200))
	}
	var ans Answer
	if err := json.Unmarshal([]byte(m), &ans); err != nil {
		return Answer{}, fmt.Errorf("acp decider: %v in %q", err, m)
	}
	return ans, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// runACP runs one prompt in a new session and returns the agent's text.
func runACP(ctx context.Context, command []string, prompt string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = os.Environ()
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("acp decider: cannot start %q: %w", command[0], err)
	}
	defer func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	cl := &acpClient{}
	conn := acp.NewClientSideConnection(cl, stdin, stdout)
	conn.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo: &acp.Implementation{Name: "artefact", Version: "0.1.0"}}); err != nil {
		return "", fmt.Errorf("acp decider: initialize: %w", err)
	}
	cwd, _ := os.Getwd()
	sess, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		return "", fmt.Errorf("acp decider: session/new: %w", err)
	}
	if _, err := conn.Prompt(ctx, acp.PromptRequest{SessionId: sess.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock(prompt)}}); err != nil {
		return "", fmt.Errorf("acp decider: session/prompt: %w", err)
	}
	return cl.text(), nil
}

type acpClient struct {
	mu  sync.Mutex
	buf strings.Builder
}

var _ acp.Client = (*acpClient)(nil)

func (c *acpClient) text() string { c.mu.Lock(); defer c.mu.Unlock(); return c.buf.String() }

// RequestPermission allows reading only: the decider changes nothing.
func (c *acpClient) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	pick := ""
	readOnly := p.ToolCall.Kind != nil && (*p.ToolCall.Kind == acp.ToolKindRead || *p.ToolCall.Kind == acp.ToolKindSearch)
	for _, o := range p.Options {
		if readOnly && o.Kind == acp.PermissionOptionKindAllowOnce && pick == "" {
			pick = string(o.OptionId)
		}
		if !readOnly && o.Kind == acp.PermissionOptionKindRejectOnce && pick == "" {
			pick = string(o.OptionId)
		}
	}
	if pick == "" {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Cancelled: &acp.RequestPermissionOutcomeCancelled{}}}, nil
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{Selected: &acp.RequestPermissionOutcomeSelected{OptionId: acp.PermissionOptionId(pick)}}}, nil
}

func (c *acpClient) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	if u := n.Update; u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil {
		c.mu.Lock()
		c.buf.WriteString(u.AgentMessageChunk.Content.Text.Text)
		c.mu.Unlock()
	}
	return nil
}

var errNoTool = errors.New("artefact offers no client tool")

func (c *acpClient) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, errNoTool
}
func (c *acpClient) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, errNoTool
}
func (c *acpClient) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, errNoTool
}
func (c *acpClient) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, errNoTool
}
func (c *acpClient) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, errNoTool
}
func (c *acpClient) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, errNoTool
}
func (c *acpClient) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, errNoTool
}

// Step records one level of a walk.
type Step struct {
	Level      string             `json:"level"` // notation of the parent, "" at the top
	Choice     string             `json:"choice"`
	Confidence float64            `json:"confidence"`
	Top        map[string]float64 `json:"top,omitempty"`
}

// Outcome is where a walk stopped.
type Outcome struct {
	Node       string  `json:"node"` // "" when no class could be chosen
	Confidence float64 `json:"confidence"`
	Pending    bool    `json:"pending"` // stopped by low confidence: a human decides
	Steps      []Step  `json:"steps"`
}

// Walk descends the plan one level at a time. It stops at a node whose
// children do not fit ("none" with confidence) or when confidence falls
// below threshold, which leaves the decision to a human.
func Walk(ctx context.Context, p *Plan, f Facts, d Decider, threshold float64) (Outcome, error) {
	var out Outcome
	level := p.Nodes
	parent, pathLabels := "", []string{}
	conf := 1.0
	for len(level) > 0 {
		q := Question{Plan: p.Title, Path: strings.Join(pathLabels, " › "), Facts: f}
		for _, n := range level {
			q.Candidates = append(q.Candidates, Candidate{Key: n.Notation, Label: n.Label, Scope: n.Scope})
		}
		a, err := d.Decide(ctx, q)
		if err != nil {
			return out, err
		}
		out.Steps = append(out.Steps, Step{Level: parent, Choice: a.Choice, Confidence: a.Confidence, Top: top(a.Probabilities, 5)})
		if a.Confidence < threshold {
			out.Pending = true
			break
		}
		var next *Node
		for _, n := range level {
			if n.Notation == a.Choice {
				next = n
			}
		}
		if next == nil { // "none", or an answer outside the candidates
			if parent == "" {
				out.Pending = true
			}
			break
		}
		parent, conf = next.Notation, min(conf, a.Confidence)
		pathLabels = append(pathLabels, next.Label)
		level = next.Children
	}
	out.Node, out.Confidence = parent, conf
	if parent == "" {
		out.Confidence = 0
	}
	return out, nil
}

func top(p map[string]float64, n int) map[string]float64 {
	if len(p) <= n {
		return p
	}
	type kv struct {
		k string
		v float64
	}
	var l []kv
	for k, v := range p {
		l = append(l, kv{k, v})
	}
	for i := range l {
		for j := i + 1; j < len(l); j++ {
			if l[j].v > l[i].v {
				l[i], l[j] = l[j], l[i]
			}
		}
	}
	out := map[string]float64{}
	for _, x := range l[:n] {
		out[x.k] = x.v
	}
	return out
}
