// Package client talks to `artefact serve` over its Unix socket or a
// loopback address. It is the contract other tools build on (SPEC §5.3, §14).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Client calls one artefact server.
type Client struct {
	base  string
	token string
	agent string
	http  *http.Client
}

// Error is the error envelope of the server.
type Error struct {
	Status  int    `json:"-"`
	Code    int    `json:"code"`
	Kind    string `json:"kind"` // user_error, not_found, forbidden, unauthorized, error
	Message string `json:"message"`
}

func (e *Error) Error() string { return "artefact: " + e.Message }

// IsNotFound reports whether err is a not_found error.
func IsNotFound(err error) bool { e, ok := err.(*Error); return ok && e.Kind == "not_found" }

type Option func(*Client)

// WithToken sends a bearer token; required over TCP, optional on the socket.
func WithToken(t string) Option { return func(c *Client) { c.token = t } }

// WithAgent names the caller in the artefacts' event logs.
func WithAgent(a string) Option { return func(c *Client) { c.agent = a } }

// DefaultSocket is ~/.local/state/artefact/sock.
func DefaultSocket() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "artefact", "sock")
}

// NewSocket returns a client of the server on the Unix socket at path.
func NewSocket(path string, opts ...Option) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
	return newClient("http://artefact", &http.Client{Transport: tr}, opts)
}

// NewHTTP returns a client of the server at base, e.g. http://127.0.0.1:7780.
func NewHTTP(base string, opts ...Option) *Client {
	return newClient(strings.TrimRight(base, "/"), &http.Client{}, opts)
}

func newClient(base string, hc *http.Client, opts []Option) *Client {
	c := &Client{base: base, http: hc}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.agent != "" {
		req.Header.Set("X-Artefact-Agent", c.agent)
	}
	return c.http.Do(req.WithContext(ctx))
}

func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("artefact: unreadable answer (HTTP %d): %w", resp.StatusCode, err)
	}
	if !env.OK {
		if env.Error == nil {
			env.Error = &Error{Kind: "error", Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
		}
		env.Error.Status = resp.StatusCode
		return env.Error
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// Call runs any action with JSON parameters and decodes its result into out.
func (c *Client) Call(ctx context.Context, action string, params map[string]any, out any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/"+action, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}
	return decode(resp, out)
}

type Holder struct {
	By  string `json:"by"`
	Ref string `json:"ref,omitempty"`
	At  string `json:"at,omitempty"`
}

type PutOptions struct {
	Sphere   string
	Filename string
	Title    string
	Date     string // YYYY-MM-DD
	Mime     string
	Holds    []Holder
	Card     string // YAML of editable card fields
	Key      string // idempotency key, scoped to the sphere
	Seal     bool
}

type PutResult struct {
	ID      string   `json:"id"`
	URI     string   `json:"uri"`
	Sphere  string   `json:"sphere"`
	Version int      `json:"version"`
	Status  string   `json:"status"` // created | known
	Digest  string   `json:"digest"`
	Size    int64    `json:"size"`
	Sealed  bool     `json:"sealed"`
	Title   string   `json:"title"`
	Holders []Holder `json:"holders"`
}

// Put deposits the payload read from r.
func (c *Client) Put(ctx context.Context, r io.Reader, o PutOptions) (*PutResult, error) {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("sphere", o.Sphere)
	set("filename", o.Filename)
	set("title", o.Title)
	set("date", o.Date)
	set("mime", o.Mime)
	set("card", o.Card)
	set("key", o.Key)
	if o.Seal {
		q.Set("seal", "true")
	}
	for _, h := range o.Holds {
		q.Add("hold", h.By+":"+h.Ref)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/put?"+q.Encode(), r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	var out PutResult
	return &out, decode(resp, &out)
}

// PutFile deposits a file of this machine; its name becomes the filename.
func (c *Client) PutFile(ctx context.Context, path string, o PutOptions) (*PutResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if o.Filename == "" {
		o.Filename = filepath.Base(path)
	}
	return c.Put(ctx, f, o)
}

// Open streams the payload of id, a URI or a bare ULID (version "" for the
// current one). A bare ULID costs one stat call to find its sphere.
func (c *Client) Open(ctx context.Context, id, version string, reveal bool) (io.ReadCloser, error) {
	sphere, ulid, ok := splitURI(id)
	if !ok {
		st, missing, err := c.Stat(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(missing) > 0 || len(st) == 0 {
			return nil, &Error{Kind: "not_found", Message: "no artefact " + id}
		}
		if sphere, ulid, ok = splitURI(st[0].URI); !ok {
			return nil, &Error{Kind: "error", Message: "unexpected URI " + st[0].URI}
		}
	}
	u := c.base + "/" + sphere + "/a/" + ulid
	if version != "" {
		u += "/" + version
	}
	if reveal {
		u += "?reveal=1"
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") && resp.StatusCode != http.StatusOK {
		return nil, decode(resp, nil)
	}
	return resp.Body, nil
}

// GetFile writes the payload of id to path with mode 0600; with reveal, a
// sealed artefact is decrypted there.
func (c *Client) GetFile(ctx context.Context, id, path string, reveal bool) error {
	rc, err := c.Open(ctx, id, "", reveal)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

func splitURI(id string) (sphere, ulid string, ok bool) {
	rest, ok := strings.CutPrefix(id, "artefact://")
	if !ok {
		return "", "", false
	}
	sphere, ulid, ok = strings.Cut(rest, "/")
	return sphere, ulid, ok && sphere != "" && ulid != ""
}

type Card struct {
	ID        string         `json:"id"`
	Sphere    string         `json:"sphere"`
	Title     string         `json:"title"`
	Date      string         `json:"date"`
	Mime      string         `json:"mime"`
	Size      int64          `json:"size"`
	Filename  string         `json:"filename,omitempty"`
	Version   int            `json:"version"`
	Digest    string         `json:"digest"`
	Sealed    bool           `json:"sealed"`
	Deposited string         `json:"deposited"`
	Updated   string         `json:"updated"`
	DC        map[string]any `json:"dc,omitempty"`
	Classes   map[string]any `json:"classes,omitempty"`
	Holders   []Holder       `json:"holders"`
	X         map[string]any `json:"x,omitempty"`
}

type Stat struct {
	URI      string `json:"uri"`
	Card     *Card  `json:"card"`
	Versions []struct {
		Version string `json:"version"`
		Created string `json:"created"`
		Digest  string `json:"digest"`
	} `json:"versions"`
}

// Stat describes artefacts in one call; unknown ids come back in missing.
func (c *Client) Stat(ctx context.Context, ids ...string) (stats []Stat, missing []string, err error) {
	var out struct {
		Artefacts []Stat   `json:"artefacts"`
		Missing   []string `json:"missing"`
	}
	err = c.Call(ctx, "stat", map[string]any{"id": ids}, &out)
	return out.Artefacts, out.Missing, err
}

type Item struct {
	ID        string `json:"id"`
	URI       string `json:"uri"`
	Sphere    string `json:"sphere"`
	Digest    string `json:"digest"`
	Title     string `json:"title"`
	Filename  string `json:"filename"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	Date      string `json:"date"`
	Version   int    `json:"version"`
	Sealed    bool   `json:"sealed"`
	Deposited string `json:"deposited"`
}

type ListOptions struct {
	Sphere   string
	HeldBy   string // tool, or tool:ref
	Orphaned bool
	Since    string
	Limit    int // 0 for all
}

func (c *Client) List(ctx context.Context, o ListOptions) ([]Item, error) {
	p := map[string]any{"limit": fmt.Sprint(o.Limit)}
	if o.Sphere != "" {
		p["sphere"] = o.Sphere
	}
	if o.HeldBy != "" {
		p["held-by"] = o.HeldBy
	}
	if o.Orphaned {
		p["orphaned"] = true
	}
	if o.Since != "" {
		p["since"] = o.Since
	}
	var out []Item
	return out, c.Call(ctx, "ls", p, &out)
}

func (c *Client) Hold(ctx context.Context, id, by, ref string) error {
	return c.Call(ctx, "hold", map[string]any{"id": id, "by": by, "ref": ref}, nil)
}

// Release removes the holders of by (only ref when given) and returns how many.
func (c *Client) Release(ctx context.Context, id, by, ref string) (int, error) {
	var out struct {
		Released int `json:"released"`
	}
	p := map[string]any{"id": id, "by": by}
	if ref != "" {
		p["ref"] = ref
	}
	err := c.Call(ctx, "release", p, &out)
	return out.Released, err
}

type SealResult struct {
	ID             string   `json:"id"`
	URI            string   `json:"uri"`
	Status         string   `json:"status"`
	VersionsErased int      `json:"versions_erased"`
	Digest         string   `json:"digest"`
	ClearRemainsIn []string `json:"clear_remains_in"`
}

func (c *Client) Seal(ctx context.Context, id string) (*SealResult, error) {
	var out SealResult
	return &out, c.Call(ctx, "seal", map[string]any{"id": id}, &out)
}

// Redact adds a version from a file of this machine (Unix socket only).
func (c *Client) Redact(ctx context.Context, id, from string, purge bool) error {
	return c.Call(ctx, "redact", map[string]any{"id": id, "from": from, "purge": purge}, nil)
}

type PlanItem struct {
	URI      string   `json:"uri"`
	Title    string   `json:"title"`
	Whole    bool     `json:"whole"`
	Versions []string `json:"versions"`
	Holders  []Holder `json:"holders"`
}

type Plan struct {
	Items   []PlanItem `json:"items"`
	Digest  string     `json:"digest"`
	Code    string     `json:"code"`
	Expires string     `json:"expires"`
}

// PlanErase returns the plan and the code; nothing is erased yet.
func (c *Client) PlanErase(ctx context.Context, ids []string, version string) (*Plan, error) {
	p := map[string]any{"id": ids, "plan": true}
	if version != "" {
		p["version"] = version
	}
	var out Plan
	return &out, c.Call(ctx, "erase", p, &out)
}

type Proof struct {
	Erased []struct {
		URI            string `json:"uri"`
		Whole          bool   `json:"whole"`
		VersionsErased int    `json:"versions_erased"`
	} `json:"erased"`
	Locations []struct {
		URI      string `json:"uri"`
		Backend  string `json:"backend"`
		Verified bool   `json:"verified"`
	} `json:"locations"`
	NotCovered []string `json:"not_covered"`
}

// ConfirmErase runs the plan the code was issued for.
func (c *Client) ConfirmErase(ctx context.Context, code string) (*Proof, error) {
	var out Proof
	return &out, c.Call(ctx, "erase", map[string]any{"confirm": code}, &out)
}
