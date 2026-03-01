package main

import (
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	_ "modernc.org/sqlite"
	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// Version is set at build time via -ldflags "-X main.Version=v0.2.0"
var Version = "dev"

//go:embed static/favicon.svg
var faviconSVG []byte

//go:embed static/logo.svg
var logoSVG []byte

//go:embed docs/app-help.md
var helpMD []byte

//go:embed docs/dest-url-help.md
var destURLHelpMD []byte

// logger is the structured application logger (WARN/ERROR). The default log
// package is silenced in non-verbose mode so tsnet noise is suppressed, while
// this slog-based logger remains active at all times. Informational startup
// messages use fmt.Fprintf(os.Stderr, ...) for clean operator-facing output.
var logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
	Level: slog.LevelWarn,
	ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			a.Value = slog.StringValue(a.Value.Time().Format("2006/01/02 15:04:05"))
		}
		return a
	},
}))

var (
	listeners  listenFlag
	verbose    = flag.Bool("verbose", false, "verbose tsnet logging")
	tsHostname      = flag.String("ts-hostname", "go", "Tailscale node hostname")
	tsDir           = flag.String("ts-dir", "", "Tailscale state directory (default: OS config dir)")
	importFile      = flag.String("import", "", "import linx from JSON file (skips existing)")
	resolveFile     = flag.String("resolve", "", "resolve a link from JSON backup file and exit")
	maxResolveDepth = flag.Int("max-resolve-depth", 5, "maximum link chain resolution depth")
	userPermsFlag   = flag.String("user-perms", "", `LAN user permissions: comma-separated list of "add","update","delete", or "*" for all, "" for read-only (default "*")`)
	deleteRetention = flag.Int("delete-retention", 30, "days to keep deleted items before purge (0 = keep forever)")
)

func init() {
	flag.Var(&listeners, "listen", "listener URI (repeatable)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `GoLinx - URL shortener and people directory

Usage:
  golinx [flags]
  golinx                                               (loads golinx.toml)

Listener URIs (--listen, repeatable):
  --listen "http://:80"                                Plain HTTP
  --listen "https://:443;cert=c.pem;key=k.pem"        HTTPS with own certs
  --listen "ts+https://:443"                           Tailscale HTTPS (requires --ts-hostname)
  --listen "ts+http://:80"                             Tailscale plain HTTP

Flags:
  --verbose            Verbose tsnet logging (default: false)
  --ts-hostname NAME   Tailscale node hostname (default: go)
  --ts-dir PATH        Tailscale state directory
                       (default: ~/.config/tsnet-golinx on Linux,
                        %%APPDATA%%\tsnet-golinx on Windows)
  --user-perms PERMS   LAN user permissions: comma-separated list of
                       add,update,delete or * for all (default: *)
  --import FILE        Import linx from JSON file (skips existing)
  --resolve FILE       Resolve a link from JSON backup and exit
                       Usage: golinx --resolve links.json shortname/path
  --max-resolve-depth N  Max link chain depth (default: 5)
  --delete-retention N   Days to keep deleted items before purge
                         (default: 30, 0 = keep forever)

Config:
  Place a golinx.toml in the working directory. See golinx.example.toml.
  Command-line flags override config file values.

`)
	}
}

var db *SQLiteDB

var localClient *local.Client

// peerCaps defines the shape of the app capability object in Tailscale grants.
// Grant example: {"src":["group:admins"],"dst":["tag:golinx"],"app":{"golinx.dev/cap/golinx":[{"admin":true}]}}
type peerCaps struct {
	Admin bool `json:"admin"`
}

const golinxCapName tailcfg.PeerCapability = "golinx.dev/cap/golinx"

// currentUser returns the login name and admin status of the user making the request.
// Defaults to Tailscale WhoIs lookup; overridden in Run() for non-ts modes.
var currentUser = func(r *http.Request) (login string, admin bool, err error) {
	if localClient == nil {
		return "", false, fmt.Errorf("no local client")
	}
	whois, err := localClient.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		return "", false, err
	}
	login = whois.UserProfile.LoginName
	caps, _ := tailcfg.UnmarshalCapJSON[peerCaps](whois.CapMap, golinxCapName)
	for _, cap := range caps {
		if cap.Admin {
			return login, true, nil
		}
	}
	return login, false, nil
}

// canEdit returns true if the request user is allowed to modify the linx.
// In local mode (no Tailscale), everyone can edit. On Tailscale, only the
// owner or claimants of unowned linx can edit.
func canEdit(r *http.Request, linxOwner string) bool {
	if isLocalhost(r) {
		return true
	}
	if linxOwner == "" {
		return true
	}
	login, admin, err := currentUser(r)
	if err != nil {
		return false
	}
	if login == linxOwner {
		return true
	}
	if admin {
		if mode, _ := db.GetSetting(login, "adminMode"); mode == "true" {
			return true
		}
	}
	return false
}

// isLocalhost returns true if the request originates from the loopback interface.
func isLocalhost(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host == "127.0.0.1" || host == "::1"
}

// isLocalUser returns true for non-localhost requests in local mode (no Tailscale).
func isLocalUser(r *http.Request) bool {
	return localClient == nil && !isLocalhost(r)
}

// hasUserPerm checks if the given permission is granted by the user-perms config.
func hasUserPerm(perm string) bool {
	for _, p := range userPerms {
		if p == "*" || strings.EqualFold(p, perm) {
			return true
		}
	}
	return false
}

// normalizeTags cleans a comma-separated tag string: lowercase, no spaces
// (replaced with dashes), max 30 chars per tag, max 10 tags, deduped.
func normalizeTags(raw string) string {
	parts := strings.Split(raw, ",")
	seen := make(map[string]bool)
	var tags []string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		t = strings.ToLower(t)
		t = strings.Join(strings.Fields(t), "-")
		if len(t) > 30 {
			t = t[:30]
		}
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
		if len(tags) >= 10 {
			break
		}
	}
	return strings.Join(tags, ", ")
}

var reShortName = regexp.MustCompile(`^\w[\w\-.]*$`)

// validateDestURL checks that a destination URL is acceptable.
// Accepts http/https URLs and bare paths (local aliases like "docs" or "go/docs").
func validateDestURL(dest string) error {
	u, err := url.Parse(dest)
	if err != nil {
		return fmt.Errorf("invalid destination URL")
	}
	switch u.Scheme {
	case "http", "https":
		return nil // external URL
	case "":
		// Bare path — local alias (e.g. "docs", "go/docs", "/docs")
		if u.Host == "" && u.Path != "" {
			return nil
		}
		return fmt.Errorf("destination must be a URL (http/https) or a local short name")
	default:
		return fmt.Errorf("destination must be a URL (http/https) or a local short name")
	}
}

// detectLinkLoop checks whether a link's destination URL would create a
// redirect loop through other local linx. It walks the chain up to 10 hops.
// Returns a user-friendly error message, or "" if no loop is detected.
// runImport reads a JSON file exported by /.export and inserts any linx
// whose shortName does not already exist in the database.
func runImport(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading import file: %w", err)
	}
	var items []Linx
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("parsing import file: %w", err)
	}
	if len(items) == 0 {
		fmt.Println("no linx found in import file")
		return nil
	}

	var added, skipped int
	for _, c := range items {
		if c.ShortName == "" {
			continue
		}
		if _, err := db.LoadByShortName(c.ShortName); err == nil {
			skipped++
			fmt.Printf("  skip: /%s (already exists)\n", c.ShortName)
			continue
		}
		// Clear ID so the database assigns a new one.
		c.ID = 0
		if _, err := db.Save(&c); err != nil {
			fmt.Printf("  FAIL: /%s (%v)\n", c.ShortName, err)
			continue
		}
		added++
		fmt.Printf("  added: /%s\n", c.ShortName)
	}
	fmt.Printf("\nimport complete: %d added, %d skipped\n", added, skipped)
	return nil
}

// runResolve loads a JSON backup into an in-memory SQLite database and
// resolves a link using the exact same code path as the live server.
func runResolve(path, link string) error {
	// Open in-memory database — same schema, same logic, no file on disk.
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		return fmt.Errorf("in-memory DB: %w", err)
	}

	// Load backup into the database via the same import logic.
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading backup file: %w", err)
	}
	var items []Linx
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("parsing backup file: %w", err)
	}
	for i := range items {
		items[i].ID = 0
		db.Save(&items[i])
	}
	fmt.Printf("loaded %d linx from %s\n\n", len(items), path)

	// Parse the link argument.
	link = strings.TrimPrefix(link, "/")
	short, remainder, _ := strings.Cut(link, "/")

	lnx, err := db.LoadByShortName(short)
	if errors.Is(err, fs.ErrNotExist) {
		if s := strings.TrimRight(short, ".,()[]{}"); s != short {
			short = s
			lnx, err = db.LoadByShortName(short)
		}
	}
	if err != nil {
		return fmt.Errorf("/%s not found in backup", short)
	}
	if lnx.IsPersonType() {
		fmt.Printf("/%s is a %s profile: %s %s\n", lnx.ShortName, lnx.Type, lnx.FirstName, lnx.LastName)
		return nil
	}
	if lnx.IsDocumentType() {
		fmt.Printf("/%s is a document: %s\n", lnx.ShortName, lnx.Description)
		return nil
	}

	// Expand using the same logic as serveRedirect.
	env := expandEnv{
		Now:  time.Now().UTC(),
		Path: remainder,
	}
	target, err := expandLink(lnx.DestinationURL, env)
	if err != nil {
		return fmt.Errorf("expanding /%s: %w", short, err)
	}

	// Follow local link chains up to the configured depth.
	// Only follow relative URLs (no host) to avoid false matches
	// against external URL paths (e.g. https://google.com/search).
	for i := 0; i < *maxResolveDepth; i++ {
		if target.Host != "" {
			break
		}
		nextShort := extractLocalShortName(target)
		if nextShort == "" {
			break
		}
		next, err := db.LoadByShortName(nextShort)
		if err != nil || next.Type != LinxTypeLink {
			break
		}
		nextTarget, err := expandLink(next.DestinationURL, expandEnv{Now: time.Now().UTC()})
		if err != nil {
			break
		}
		target = nextTarget
	}

	fmt.Println(target.String())
	return nil
}

// extractLocalShortName returns the short name from a URL if it looks like
// a simple /<shortname> local path, or "" otherwise.
func extractLocalShortName(u *url.URL) string {
	p := strings.TrimPrefix(u.Path, "/")
	if p == "" || strings.Contains(p, "/") {
		return ""
	}
	return p
}

func detectLinkLoop(shortName, destURL string) string {
	u, err := url.Parse(destURL)
	if err != nil {
		return ""
	}
	target := extractLocalShortName(u)
	if target == "" {
		return ""
	}

	// Direct self-loop: /foo → /foo
	if strings.EqualFold(target, shortName) {
		return fmt.Sprintf("link loop detected: /%s points back to itself", shortName)
	}

	// Walk the chain through local linx.
	seen := map[string]bool{strings.ToLower(shortName): true}
	chain := []string{shortName}
	current := target
	for i := 0; i < *maxResolveDepth; i++ {
		lower := strings.ToLower(current)
		if seen[lower] {
			chain = append(chain, current)
			return fmt.Sprintf("link loop detected: /%s", strings.Join(chain, " → /"))
		}
		lnx, err := db.LoadByShortName(current)
		if err != nil || lnx.Type != LinxTypeLink || lnx.DestinationURL == "" {
			return "" // chain ends at a non-existent or non-link linx
		}
		seen[lower] = true
		chain = append(chain, current)
		nu, err := url.Parse(lnx.DestinationURL)
		if err != nil {
			return ""
		}
		next := extractLocalShortName(nu)
		if next == "" {
			return "" // destination is an external URL
		}
		current = next
	}
	return ""
}

// listenFlag collects repeatable --listen values.
type listenFlag []string

func (f *listenFlag) String() string { return strings.Join(*f, ", ") }
func (f *listenFlag) Set(val string) error {
	*f = append(*f, val)
	return nil
}

// config represents the golinx.toml configuration file.
type config struct {
	Listen          []string `toml:"listen"`
	Verbose         bool     `toml:"verbose"`
	TailscaleHost   string   `toml:"ts-hostname"`
	TailscaleDir    string   `toml:"ts-dir"`
	MaxResolveDepth int      `toml:"max-resolve-depth"`
	UserPerms       []string `toml:"user-perms"`
	DeleteRetention *int     `toml:"delete-retention"`
}

var userPerms = []string{"*"} // default: full access for local users

const configFile = "golinx.toml"

// loadConfig loads golinx.toml from the working directory if it exists.
// Returns the config and true if the file was found, or a zero config and
// false if it does not exist.
func loadConfig() (config, bool, error) {
	var cfg config
	_, err := toml.DecodeFile(configFile, &cfg)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, false, nil
		}
		return cfg, false, fmt.Errorf("loading %s: %w", configFile, err)
	}
	fmt.Fprintf(os.Stderr, "Loaded config from %s\n", configFile)
	return cfg, true, nil
}

// listener is a parsed --listen URI.
type listener struct {
	scheme   string // "http", "https", "ts+http", "ts+https"
	host     string // bind address (IP or empty)
	port     string // port number
	certFile string // https only
	keyFile  string // https only
}

// addr returns host:port for use with net listeners.
func (l listener) addr() string {
	return net.JoinHostPort(l.host, l.port)
}

// parseListener parses a --listen URI like "http://:8080",
// "https://:443;cert=c.pem;key=k.pem", "ts+https://:443", or "ts+http://:80".
// Host must be empty or an IP address — hostnames are not allowed.
func parseListener(raw string) (listener, error) {
	// Split scheme from rest.
	idx := strings.Index(raw, "://")
	if idx < 0 {
		return listener{}, fmt.Errorf("invalid listener %q: missing scheme (http://, https://, ts+http://, ts+https://)", raw)
	}
	scheme := strings.ToLower(raw[:idx])
	rest := raw[idx+3:]

	switch scheme {
	case "http", "https", "ts+http", "ts+https":
	default:
		return listener{}, fmt.Errorf("invalid listener %q: unknown scheme %q (use http, https, ts+http, or ts+https)", raw, scheme)
	}

	// Split host:port from ;params.
	parts := strings.Split(rest, ";")
	hostPort := parts[0]

	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return listener{}, fmt.Errorf("invalid listener %q: %w", raw, err)
	}
	if port == "" {
		return listener{}, fmt.Errorf("invalid listener %q: port is required", raw)
	}

	// Host must be empty (bind all) or a valid IP address — no hostnames.
	if host != "" && net.ParseIP(host) == nil {
		return listener{}, fmt.Errorf("invalid listener %q: host must be an IP address or empty (got %q)", raw, host)
	}

	// Parse ;key=value parameters.
	params := make(map[string]string)
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return listener{}, fmt.Errorf("invalid listener %q: bad parameter %q (expected key=value)", raw, p)
		}
		params[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}

	l := listener{scheme: scheme, host: host, port: port}

	if scheme == "https" {
		l.certFile = params["cert"]
		l.keyFile = params["key"]
		if l.certFile == "" || l.keyFile == "" {
			return listener{}, fmt.Errorf("invalid listener %q: https:// requires cert and key parameters", raw)
		}
	}

	return l, nil
}

// validateListeners checks that the parsed listeners are valid.
func validateListeners(parsed []listener) error {
	if len(parsed) == 0 {
		return fmt.Errorf("at least one --listen URI is required")
	}

	hasTS := false
	for _, l := range parsed {
		if strings.HasPrefix(l.scheme, "ts+") {
			hasTS = true
		}
		if l.scheme == "https" {
			if _, err := tls.LoadX509KeyPair(l.certFile, l.keyFile); err != nil {
				return fmt.Errorf("loading TLS certificate for %s: %w", l.addr(), err)
			}
		}
	}
	if hasTS {
		if err := validateTSHostname(*tsHostname); err != nil {
			return err
		}
	}
	return nil
}

// reTSHostname matches a valid Tailscale hostname (DNS label):
// starts with a letter or digit, may contain hyphens, max 63 chars.
var reTSHostname = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// validateTSHostname checks that the ts-hostname is set and valid.
func validateTSHostname(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("--ts-hostname is required when using ts+http:// or ts+https:// listeners")
	}
	if !reTSHostname.MatchString(name) {
		return fmt.Errorf("--ts-hostname %q is not a valid hostname (letters, digits, hyphens; must start and end with a letter or digit; max 63 chars)", name)
	}
	return nil
}

// Run is the main entry point for the application.
// seedDefaults creates sample linx on first run (empty DB) so new users
// have something to see instead of an empty grid.
func seedDefaults() {
	count, err := db.LinxCount("")
	if err != nil || count > 0 {
		return
	}

	// Google search — the link everyone adds eventually.
	db.Save(&Linx{
		Type:           LinxTypeLink,
		ShortName:      "search",
		DestinationURL: "https://www.google.com/search?q={{.Path}}",
		Description:    "Google Search (try go/search/how to mass-customize a link shortener)",
	})

	// First employee.
	db.Save(&Linx{
		Type:      LinxTypeEmployee,
		ShortName: "ceo",
		FirstName: "Linky",
		LastName:  "McShortlink",
		Title:     "Chief Everything Officer",
		Email:     "linky@localhost",
	})

	// Our Mission document (Easter egg).
	id, err := db.Save(&Linx{
		Type:        LinxTypeDocument,
		ShortName:   "mission",
		Description: "Our Mission",
	})
	if err == nil {
		db.SaveDocument(id, []byte(missionMarkdown), "text/markdown")
	}

	// Performance review — HTML showcase.
	id, err = db.Save(&Linx{
		Type:        LinxTypeDocument,
		ShortName:   "review",
		Description: "Q1 Performance Review: Linky McShortlink",
	})
	if err == nil {
		db.SaveDocument(id, []byte(reviewHTML), "text/html")
	}
}

const missionMarkdown = `# Our Mission

Our mission is simple: to be the absolute best URL shortener the world has ever seen.

While others are out there curing diseases and solving climate change, we're laser-focused on what *truly* matters — turning long URLs into short ones. You're welcome.

## Core Values

- **Move Fast and Redirect Things** — Every millisecond counts. Our redirects are so fast they finish before they start.
- **Radical Shortness** — We believe every URL deserves to be shorter. Even the short ones.
- **Synergy** — We don't know what this means, but our investors love it.
- **Innovation** — We put a link shortener on your Tailnet. Nobody asked for this. You're welcome, again.

## Our Story

It started with a developer who was mass-customizing a link-shortener called "golinks."
He got carried away. Way, way carried away.
What began as a simple fork turned into a full-blown URL shortener, people directory, document reader, and whatever else seemed cool that week.

We regret nothing.

## Looking Ahead

Our five-year plan includes:

1. Continue adding features nobody requested
2. Write more TOML config options than anyone will ever read
3. Achieve mass adoption (at least 3 users)
4. IPO (Initial Proxmox Offering)

*If you're reading this, you've found our Easter egg. Feel free to delete this and create something useful. Or don't. We're not your boss.*
`

const reviewHTML = `<h1>Q1 Performance Review</h1>
<p><strong>Employee:</strong> Linky McShortlink<br>
<strong>Title:</strong> Chief Everything Officer<br>
<strong>Review Period:</strong> January 1 – March 31<br>
<strong>Reviewer:</strong> The Entire Internet</p>

<hr>

<h2>Performance Summary</h2>

<table>
<thead>
<tr><th>Category</th><th>Rating</th><th>Comments</th></tr>
</thead>
<tbody>
<tr><td>URL Shortening</td><td>Exceeds Expectations</td><td>Has never once made a URL longer. Flawless record.</td></tr>
<tr><td>Redirect Speed</td><td>Outstanding</td><td>301s so fast, photons feel slow.</td></tr>
<tr><td>Feature Creep</td><td>Needs Improvement</td><td>Was hired to shorten URLs. Now runs a people directory, document reader, and ping utility. Nobody asked for any of this.</td></tr>
<tr><td>Work-Life Balance</td><td>Not Applicable</td><td>Is a binary. Does not sleep. Does not eat. Restarts only when told.</td></tr>
<tr><td>Team Collaboration</td><td>Meets Expectations</td><td>Works well with SQLite. Tolerates Tailscale. Ignores Redis entirely.</td></tr>
</tbody>
</table>

<h2>Key Accomplishments</h2>

<ul>
<li>Successfully redirected <strong>tens</strong> of URLs (we're still in early adoption)</li>
<li>Survived multiple <code>kill -9</code> attempts with zero complaints</li>
<li>Maintained 100% uptime during periods when no one was looking</li>
<li>Learned to render Markdown despite being a URL shortener</li>
<li>Achieved <em>mass adoption</em> across 1–3 devices</li>
</ul>

<h2>Areas for Growth</h2>

<ol>
<li><strong>Scope management</strong> — When asked to add "just one more feature," practice saying no. You are a link shortener. Act like it.</li>
<li><strong>User acquisition</strong> — Current user base fits comfortably in a Proxmox container. And a small one.</li>
<li><strong>Documentation</strong> — The config file has more comments than the codebase. This is somehow both impressive and concerning.</li>
</ol>

<h2>Peer Feedback</h2>

<blockquote>
<p>"Linky is the hardest-working single binary I've ever deployed. Always there when I need a redirect. Never judges my browsing habits." — <em>Anonymous Tailnet User</em></p>
</blockquote>

<blockquote>
<p>"I asked for a URL shortener. I got an entire SaaS platform in a 15MB executable. I'm not mad, just confused." — <em>The Developer</em></p>
</blockquote>

<blockquote>
<p>"SELECT * FROM Linx WHERE appreciation = 'maximum'" — <em>SQLite</em></p>
</blockquote>

<h2>Goals for Next Quarter</h2>

<table>
<thead>
<tr><th>Goal</th><th>Priority</th><th>Likelihood</th></tr>
</thead>
<tbody>
<tr><td>Resist adding a built-in email server</td><td>Critical</td><td>Low</td></tr>
<tr><td>Get a second user</td><td>High</td><td>Moderate</td></tr>
<tr><td>Update the README before it becomes historical fiction</td><td>Medium</td><td>Uncertain</td></tr>
<tr><td>Achieve sentience</td><td>Stretch</td><td>Pending TOML config option</td></tr>
</tbody>
</table>

<hr>

<p><strong>Overall Rating: Exceeds Expectations</strong></p>
<p><em>Linky continues to be an invaluable member of the team, primarily because Linky is the entire team. Keep up the mass-customizing work.</em></p>
`

func Run() error {
	// Support -? and --? as help flags.
	for _, arg := range os.Args[1:] {
		if arg == "-?" || arg == "--?" {
			flag.Usage()
			os.Exit(0)
		}
	}
	flag.Parse()

	// Load config file if present.
	cfg, hasConfig, err := loadConfig()
	if err != nil {
		return err
	}

	// Detect which CLI flags were explicitly set.
	cliSet := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { cliSet[f.Name] = true })

	// Warn if CLI flags override config file values.
	if hasConfig && len(cliSet) > 0 {
		logger.Warn("command-line flags override golinx.toml settings")
	}

	// Merge: CLI flags take precedence over config file.
	listenURIs := []string(listeners)
	if len(listenURIs) == 0 && hasConfig {
		listenURIs = cfg.Listen
	}
	if !cliSet["verbose"] && hasConfig {
		*verbose = cfg.Verbose
	}

	if !cliSet["ts-hostname"] && hasConfig && cfg.TailscaleHost != "" {
		*tsHostname = cfg.TailscaleHost
	}
	if !cliSet["ts-dir"] && hasConfig && cfg.TailscaleDir != "" {
		*tsDir = cfg.TailscaleDir
	}
	if !cliSet["max-resolve-depth"] && hasConfig && cfg.MaxResolveDepth > 0 {
		*maxResolveDepth = cfg.MaxResolveDepth
	}
	if !cliSet["delete-retention"] && hasConfig && cfg.DeleteRetention != nil {
		*deleteRetention = *cfg.DeleteRetention
	}
	if cliSet["user-perms"] {
		// CLI flag: parse comma-separated list (empty string = read-only)
		if *userPermsFlag == "" {
			userPerms = []string{}
		} else {
			parts := strings.Split(*userPermsFlag, ",")
			userPerms = make([]string, 0, len(parts))
			for _, p := range parts {
				if s := strings.TrimSpace(p); s != "" {
					userPerms = append(userPerms, s)
				}
			}
		}
	} else if hasConfig && cfg.UserPerms != nil {
		userPerms = cfg.UserPerms
	}

	// Handle --import: open DB, import, exit.
	if *importFile != "" {
		db, err = NewSQLiteDB("golinx.db")
		if err != nil {
			return fmt.Errorf("NewSQLiteDB: %w", err)
		}
		return runImport(*importFile)
	}

	// Handle --resolve: resolve link from JSON backup without starting server.
	if *resolveFile != "" {
		if flag.NArg() != 1 {
			return fmt.Errorf("--resolve requires a link argument, e.g.: golinx --resolve links.json shortname/path")
		}
		return runResolve(*resolveFile, flag.Arg(0))
	}

	// Parse listener URIs.
	parsed := make([]listener, 0, len(listenURIs))
	for _, raw := range listenURIs {
		l, err := parseListener(raw)
		if err != nil {
			return err
		}
		parsed = append(parsed, l)
	}
	if err := validateListeners(parsed); err != nil {
		return err
	}

	db, err = NewSQLiteDB("golinx.db")
	if err != nil {
		return fmt.Errorf("NewSQLiteDB: %w", err)
	}
	seedDefaults()

	// Start background purge of expired soft-deleted items.
	purgeCtx, purgeCancel := context.WithCancel(context.Background())
	defer purgeCancel()
	go startPurgeLoop(purgeCtx, *deleteRetention)

	// Silence default logger so tsnet noise is suppressed in non-verbose mode.
	if !*verbose {
		log.SetOutput(io.Discard)
	}

	// Determine identity mode.
	hasTS := false
	hasLocal := false
	for _, l := range parsed {
		if strings.HasPrefix(l.scheme, "ts+") {
			hasTS = true
		} else {
			hasLocal = true
		}
	}

	if hasTS && hasLocal {
		whoisUser := currentUser
		fallback := localIdentity()
		currentUser = func(r *http.Request) (string, bool, error) {
			if login, admin, err := whoisUser(r); err == nil {
				return login, admin, nil
			}
			return fallback, false, nil
		}
	} else if !hasTS {
		identity := localIdentity()
		currentUser = func(r *http.Request) (string, bool, error) {
			return identity, false, nil
		}
	}

	httpHandler := serveHandler()
	errCh := make(chan error, len(parsed)+2)
	var servers []*http.Server
	var tsSrv *tsnet.Server

	// Check if any HTTPS listener exists (for HTTP→HTTPS redirect).
	// Local http:// redirects to local https://; ts+http:// redirects to ts+https://.
	var localHTTPSAddr string
	var tsFQDN string
	for _, l := range parsed {
		if l.scheme == "https" && localHTTPSAddr == "" {
			localHTTPSAddr = l.addr()
		}
	}

	// Start tsnet node if any ts+* listeners exist.
	if hasTS {
		var canHTTPS bool
		tsSrv, canHTTPS, tsFQDN, err = initTailscale()
		if err != nil {
			return err
		}

		// Verify HTTPS capability if any ts+https listener is configured.
		for _, l := range parsed {
			if l.scheme == "ts+https" && !canHTTPS {
				tsSrv.Close()
				return fmt.Errorf("ts+https://%s requested but tailnet does not support HTTPS; enable HTTPS Certificates in Tailscale DNS settings", l.addr())
			}
		}
	}

	// Does a ts+https listener exist? (for ts+http redirect)
	hasTSHTTPS := false
	for _, l := range parsed {
		if l.scheme == "ts+https" {
			hasTSHTTPS = true
			break
		}
	}

	for _, l := range parsed {
		switch l.scheme {
		case "ts+https":
			ln, err := tsSrv.ListenTLS("tcp", ":"+l.port)
			if err != nil {
				tsSrv.Close()
				return fmt.Errorf("tsnet listen TLS :%s: %w", l.port, err)
			}
			fmt.Fprintf(os.Stderr, "Serving ts+https on https://%s:%s/\n", tsFQDN, l.port)
			go func() {
				if err := http.Serve(ln, hstsHandler(httpHandler)); err != nil && !errors.Is(err, net.ErrClosed) {
					errCh <- fmt.Errorf("tsnet HTTPS :%s: %w", l.port, err)
				}
			}()
		case "ts+http":
			ln, err := tsSrv.Listen("tcp", ":"+l.port)
			if err != nil {
				tsSrv.Close()
				return fmt.Errorf("tsnet listen :%s: %w", l.port, err)
			}
			var handler http.Handler
			if hasTSHTTPS {
				handler = httpsRedirectHandler(tsFQDN)
			} else {
				handler = httpHandler
			}
			fmt.Fprintf(os.Stderr, "Serving ts+http on http://%s:%s/\n", tsFQDN, l.port)
			go func(h http.Handler) {
				if err := http.Serve(ln, h); err != nil && !errors.Is(err, net.ErrClosed) {
					errCh <- fmt.Errorf("tsnet HTTP :%s: %w", l.port, err)
				}
			}(handler)
		case "https":
			srv := startLocalTLS(l, httpHandler, errCh)
			servers = append(servers, srv)
		case "http":
			var handler http.Handler
			if localHTTPSAddr != "" {
				handler = localHTTPSRedirectHandler(localHTTPSAddr)
			} else {
				handler = httpHandler
			}
			srv := startLocalHTTP(l, handler, errCh)
			servers = append(servers, srv)
		}
	}

	fmt.Fprintln(os.Stderr, "Running... Press Ctrl+C to quit.")
	return awaitShutdown(errCh, servers, tsSrv)
}

// localIdentity returns a fixed identity string for non-Tailscale requests.
func localIdentity() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "localhost"
	}
	return "local@" + h
}

// initTailscale starts the tsnet node, waits for the tailnet connection,
// and checks HTTPS capability. Returns the server, whether HTTPS is available,
// and the node's FQDN.
func initTailscale() (*tsnet.Server, bool, string, error) {
	tsSrv := &tsnet.Server{
		Hostname: *tsHostname,
		Dir:      *tsDir,
		Logf: func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			if strings.Contains(msg, "login.tailscale") || strings.Contains(msg, "To authenticate") {
				fmt.Fprintln(os.Stderr, msg)
			}
		},
	}
	if *verbose {
		tsSrv.Logf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}
	}
	if err := tsSrv.Start(); err != nil {
		return nil, false, "", fmt.Errorf("tsnet start: %w", err)
	}

	localClient, _ = tsSrv.LocalClient()

	// Wait for the tailnet connection to be ready.
	fmt.Fprintf(os.Stderr, "Connecting to tailnet as %s ...\n", *tsHostname)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		status, err := tsSrv.Up(ctx)
		cancel()
		if err == nil && status != nil {
			break
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := localClient.Status(ctx)
	if err != nil {
		return nil, false, "", fmt.Errorf("tailscale status: %w", err)
	}

	canHTTPS := status.Self.HasCap(tailcfg.CapabilityHTTPS) && len(tsSrv.CertDomains()) > 0
	fqdn := strings.TrimSuffix(status.Self.DNSName, ".")

	return tsSrv, canHTTPS, fqdn, nil
}

// startLocalTLS starts a local HTTPS server in a goroutine.
func startLocalTLS(l listener, handler http.Handler, errCh chan<- error) *http.Server {
	srv := &http.Server{Addr: l.addr(), Handler: hstsHandler(handler)}
	fmt.Fprintln(os.Stderr, "Serving HTTPS on:")
	logListenURLs("https", l.host, l.port)
	go func() {
		if err := srv.ListenAndServeTLS(l.certFile, l.keyFile); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("local TLS: %w", err)
		}
	}()
	return srv
}

// startLocalHTTP starts a local HTTP server in a goroutine.
func startLocalHTTP(l listener, handler http.Handler, errCh chan<- error) *http.Server {
	srv := &http.Server{Addr: l.addr(), Handler: handler}
	fmt.Fprintln(os.Stderr, "Serving HTTP on:")
	logListenURLs("http", l.host, l.port)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("local HTTP: %w", err)
		}
	}()
	return srv
}

// logListenURLs prints clickable URLs for a local listener.
func logListenURLs(scheme, host, port string) {
	if host != "" && host != "0.0.0.0" && host != "::" {
		fmt.Fprintf(os.Stderr, "  %s://%s/\n", scheme, net.JoinHostPort(host, port))
		return
	}
	fmt.Fprintf(os.Stderr, "  %s://localhost:%s/\n", scheme, port)
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return
	}
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		if ipNet.IP.To4() != nil {
			fmt.Fprintf(os.Stderr, "  %s://%s/\n", scheme, net.JoinHostPort(ipNet.IP.String(), port))
		}
	}
}

// localHTTPSRedirectHandler redirects HTTP requests to the local HTTPS listener.
func localHTTPSRedirectHandler(tlsAddr string) http.Handler {
	_, port, _ := net.SplitHostPort(tlsAddr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		target := "https://" + host
		if port != "443" && port != "" {
			target += ":" + port
		}
		target += r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusFound)
	})
}

// startPurgeLoop runs an initial purge and then repeats every hour.
func startPurgeLoop(ctx context.Context, retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	purge := func() {
		cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).Unix()
		n, err := db.PurgeDeleted(cutoff)
		if err != nil {
			logger.Error("purge deleted items", "err", err)
			return
		}
		if n > 0 {
			logger.Warn("purged expired deleted items", "count", n)
		}
	}
	purge()
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			purge()
		}
	}
}

// awaitShutdown waits for SIGINT or a fatal listener error, then gracefully
// shuts down all servers.
func awaitShutdown(errCh <-chan error, servers []*http.Server, tsSrv *tsnet.Server) error {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)

	select {
	case <-quit:
		fmt.Fprintln(os.Stderr, "Shutting down...")
	case err := <-errCh:
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, srv := range servers {
		srv.Shutdown(ctx)
	}
	if tsSrv != nil {
		tsSrv.Close()
	}
	return nil
}

// httpsRedirectHandler redirects all HTTP requests to their HTTPS equivalent.
func httpsRedirectHandler(fqdn string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := &url.URL{
			Scheme:   "https",
			Host:     fqdn,
			Path:     r.URL.Path,
			RawQuery: r.URL.RawQuery,
		}
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
}

// hstsHandler wraps a handler to add Strict-Transport-Security headers.
// hstsHandler wraps a handler to add Strict-Transport-Security headers.
// Only sets HSTS for multi-label hostnames (not localhost or bare names).
func hstsHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if strings.Contains(host, ".") {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func serveHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", serveIndex)
	mux.HandleFunc("GET /api/linx", apiLinxList)
	mux.HandleFunc("POST /api/linx", apiLinxCreate)
	mux.HandleFunc("PUT /api/linx/{id}", apiLinxUpdate)
	mux.HandleFunc("DELETE /api/linx/{id}", apiLinxDelete)
	mux.HandleFunc("POST /api/linx/{id}/restore", apiLinxRestore)
	mux.HandleFunc("POST /api/linx/{id}/avatar", apiLinxAvatarUpload)
	mux.HandleFunc("GET /api/linx/{id}/avatar", apiLinxAvatarGet)
	mux.HandleFunc("POST /api/linx/{id}/document", apiLinxDocumentUpload)
	mux.HandleFunc("GET /api/linx/{id}/document", apiLinxDocumentGet)
	mux.HandleFunc("GET /api/settings", apiSettingsGet)
	mux.HandleFunc("PUT /api/settings", apiSettingsPut)
	mux.HandleFunc("GET /api/whoami", apiWhoAmI)
	mux.HandleFunc("GET /api/stats", apiStats)
	mux.HandleFunc("GET /api/db", apiDBGet)
	mux.HandleFunc("PUT /api/db", apiDBPut)
	mux.HandleFunc("GET /api/suggest", apiSuggest)
	mux.HandleFunc("GET /opensearch.xml", serveOpenSearchXML)
	mux.HandleFunc("GET /favicon.svg", serveFavicon)
	mux.HandleFunc("GET /logo.svg", serveLogo)
	mux.HandleFunc("GET /.addlinx", serveAddLink)
	mux.HandleFunc("GET /.help", serveHelp)
	mux.HandleFunc("GET /.export", serveExport)
	mux.HandleFunc("GET /.deleted", serveDeleted)
	mux.HandleFunc("GET /.ping/{host}", servePing)
	mux.HandleFunc("GET /.whoami", serveWhoAmI)
	mux.HandleFunc("GET /{path...}", serveRedirect)

	return mux
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageTemplate)
}

func serveAddLink(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/?new=1", http.StatusFound)
}

var validLinxTypes = map[string]bool{
	LinxTypeLink: true, LinxTypeEmployee: true, LinxTypeCustomer: true, LinxTypeVendor: true, LinxTypeDocument: true,
}

func apiLinxList(w http.ResponseWriter, r *http.Request) {
	filterType := r.URL.Query().Get("type")
	if filterType != "" && !validLinxTypes[filterType] {
		http.Error(w, "invalid type filter", http.StatusBadRequest)
		return
	}
	items, err := db.LoadAll(filterType)
	if err != nil {
		serverError(w, "failed to load linx", err)
		return
	}
	if items == nil {
		items = []*Linx{}
	}
	writeJSON(w, http.StatusOK, items)
}

func apiLinxCreate(w http.ResponseWriter, r *http.Request) {
	if isLocalUser(r) && !hasUserPerm("add") {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	var payload struct {
		Type           string `json:"type"`
		ShortName      string `json:"shortName"`
		DestinationURL string `json:"destinationURL"`
		Description    string `json:"description"`
		Owner          string `json:"owner"`
		FirstName    string `json:"firstName"`
		LastName     string `json:"lastName"`
		Title        string `json:"title"`
		Email        string `json:"email"`
		Phone        string `json:"phone"`
		WebLink      string `json:"webLink"`
		CalLink      string `json:"calLink"`
		XLink        string `json:"xLink"`
		LinkedInLink string `json:"linkedInLink"`
		Color        string `json:"color"`
		Tags         string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	payload.Type = strings.TrimSpace(strings.ToLower(payload.Type))
	if payload.Type == "" {
		payload.Type = LinxTypeLink
	}
	payload.ShortName = strings.TrimSpace(payload.ShortName)
	payload.DestinationURL = strings.TrimSpace(payload.DestinationURL)
	payload.Description = strings.TrimSpace(payload.Description)
	payload.Owner = strings.TrimSpace(payload.Owner)
	if localClient != nil {
		// On Tailscale, always set owner to the authenticated user.
		if login, _, err := currentUser(r); err == nil {
			payload.Owner = login
		}
	} else if payload.Owner == "" {
		if login, _, err := currentUser(r); err == nil {
			payload.Owner = login
		}
	}
	payload.FirstName = strings.TrimSpace(payload.FirstName)
	payload.LastName = strings.TrimSpace(payload.LastName)
	payload.Title = strings.TrimSpace(payload.Title)
	payload.Email = strings.TrimSpace(payload.Email)
	payload.Phone = strings.TrimSpace(payload.Phone)
	payload.WebLink = strings.TrimSpace(payload.WebLink)
	payload.CalLink = strings.TrimSpace(payload.CalLink)
	payload.XLink = strings.TrimSpace(payload.XLink)
	payload.LinkedInLink = strings.TrimSpace(payload.LinkedInLink)
	payload.Color = strings.TrimSpace(payload.Color)
	payload.Tags = normalizeTags(payload.Tags)

	if !validLinxTypes[payload.Type] {
		http.Error(w, "invalid type", http.StatusBadRequest)
		return
	}
	if payload.ShortName == "" {
		http.Error(w, "shortName is required", http.StatusBadRequest)
		return
	}
	if !reShortName.MatchString(payload.ShortName) {
		http.Error(w, "shortName may only contain letters, numbers, dash, underscore, and period", http.StatusBadRequest)
		return
	}

	if payload.Type == LinxTypeLink {
		if payload.DestinationURL == "" {
			http.Error(w, "destinationURL is required for links", http.StatusBadRequest)
			return
		}
		if err := validateDestURL(payload.DestinationURL); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if msg := detectLinkLoop(payload.ShortName, payload.DestinationURL); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	} else if payload.Type == LinxTypeDocument {
		if payload.Description == "" {
			http.Error(w, "description (title) is required for documents", http.StatusBadRequest)
			return
		}
	} else {
		if payload.FirstName == "" {
			http.Error(w, "firstName is required", http.StatusBadRequest)
			return
		}
	}

	lnx := &Linx{
		Type: payload.Type, ShortName: payload.ShortName,
		DestinationURL: payload.DestinationURL, Description: payload.Description, Owner: payload.Owner,
		FirstName: payload.FirstName, LastName: payload.LastName, Title: payload.Title,
		Email: payload.Email, Phone: payload.Phone,
		WebLink: payload.WebLink, CalLink: payload.CalLink, XLink: payload.XLink, LinkedInLink: payload.LinkedInLink,
		Color: payload.Color, Tags: payload.Tags,
	}
	id, err := db.Save(lnx)
	if err != nil {
		http.Error(w, "could not create linx (short name may already exist)", http.StatusConflict)
		return
	}
	created, err := db.LoadByID(id)
	if err != nil {
		serverError(w, "failed to load created linx", err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func apiLinxUpdate(w http.ResponseWriter, r *http.Request) {
	if isLocalUser(r) && !hasUserPerm("update") {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	existing, err := db.LoadByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !canEdit(r, existing.Owner) {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}

	var payload struct {
		Type           string `json:"type"`
		ShortName      string `json:"shortName"`
		DestinationURL string `json:"destinationURL"`
		Description    string `json:"description"`
		Owner          string `json:"owner"`
		FirstName      string `json:"firstName"`
		LastName       string `json:"lastName"`
		Title          string `json:"title"`
		Email          string `json:"email"`
		Phone          string `json:"phone"`
		WebLink        string `json:"webLink"`
		CalLink        string `json:"calLink"`
		XLink          string `json:"xLink"`
		LinkedInLink   string `json:"linkedInLink"`
		Color          string `json:"color"`
		Tags           string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	payload.Type = strings.TrimSpace(strings.ToLower(payload.Type))
	if payload.Type == "" {
		payload.Type = existing.Type
	}
	payload.ShortName = strings.TrimSpace(payload.ShortName)

	if !validLinxTypes[payload.Type] {
		http.Error(w, "invalid type", http.StatusBadRequest)
		return
	}
	if payload.ShortName == "" {
		http.Error(w, "shortName is required", http.StatusBadRequest)
		return
	}
	if !reShortName.MatchString(payload.ShortName) {
		http.Error(w, "shortName may only contain letters, numbers, dash, underscore, and period", http.StatusBadRequest)
		return
	}

	payload.DestinationURL = strings.TrimSpace(payload.DestinationURL)
	payload.Description = strings.TrimSpace(payload.Description)
	payload.Owner = strings.TrimSpace(payload.Owner)
	// Claim unowned linx: if the linx had no owner, set to current user.
	if existing.Owner == "" && payload.Owner == "" {
		if login, _, err := currentUser(r); err == nil {
			payload.Owner = login
		}
	}
	payload.FirstName = strings.TrimSpace(payload.FirstName)
	payload.LastName = strings.TrimSpace(payload.LastName)
	payload.Title = strings.TrimSpace(payload.Title)
	payload.Email = strings.TrimSpace(payload.Email)
	payload.Phone = strings.TrimSpace(payload.Phone)
	payload.WebLink = strings.TrimSpace(payload.WebLink)
	payload.CalLink = strings.TrimSpace(payload.CalLink)
	payload.XLink = strings.TrimSpace(payload.XLink)
	payload.LinkedInLink = strings.TrimSpace(payload.LinkedInLink)
	payload.Color = strings.TrimSpace(payload.Color)
	payload.Tags = normalizeTags(payload.Tags)

	if payload.Type == LinxTypeLink {
		if payload.DestinationURL == "" {
			http.Error(w, "destinationURL is required for links", http.StatusBadRequest)
			return
		}
		if err := validateDestURL(payload.DestinationURL); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if msg := detectLinkLoop(payload.ShortName, payload.DestinationURL); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
	} else if payload.Type == LinxTypeDocument {
		if payload.Description == "" {
			http.Error(w, "description (title) is required for documents", http.StatusBadRequest)
			return
		}
	} else {
		if payload.FirstName == "" {
			http.Error(w, "firstName is required", http.StatusBadRequest)
			return
		}
	}

	lnx := &Linx{
		ID: id, Type: payload.Type, ShortName: payload.ShortName,
		DestinationURL: payload.DestinationURL, Description: payload.Description, Owner: payload.Owner,
		FirstName: payload.FirstName, LastName: payload.LastName, Title: payload.Title,
		Email: payload.Email, Phone: payload.Phone,
		WebLink: payload.WebLink, CalLink: payload.CalLink, XLink: payload.XLink, LinkedInLink: payload.LinkedInLink,
		Color: payload.Color, Tags: payload.Tags,
	}
	if err := db.Update(lnx); err != nil {
		if err == fs.ErrNotExist {
			http.Error(w, "linx not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	updated, err := db.LoadByID(id)
	if err != nil {
		serverError(w, "failed to load updated linx", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func apiLinxDelete(w http.ResponseWriter, r *http.Request) {
	if isLocalUser(r) && !hasUserPerm("delete") {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	existing, err := db.LoadByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !canEdit(r, existing.Owner) {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	if err := db.Delete(id); err != nil {
		if err == fs.ErrNotExist {
			http.Error(w, "linx not found", http.StatusNotFound)
			return
		}
		serverError(w, "failed to delete linx", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiLinxRestore(w http.ResponseWriter, r *http.Request) {
	if isLocalUser(r) && !hasUserPerm("delete") {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	existing, err := db.LoadByID(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if existing.DeletedAt == 0 {
		http.Error(w, "item is not deleted", http.StatusBadRequest)
		return
	}
	if !canEdit(r, existing.Owner) {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	// Check if the short name is now occupied by an active item.
	if conflict, err := db.LoadByShortName(existing.ShortName); err == nil && conflict.ID != existing.ID {
		http.Error(w, "short name is already in use", http.StatusConflict)
		return
	}
	if err := db.Restore(id); err != nil {
		if err == fs.ErrNotExist {
			http.Error(w, "item not found or not deleted", http.StatusNotFound)
			return
		}
		serverError(w, "failed to restore linx", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func settingsUser(r *http.Request) string {
	if login, _, err := currentUser(r); err == nil && login != "" {
		return login
	}
	return "default"
}

func apiSettingsGet(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	value, err := db.GetSetting(settingsUser(r), key)
	if err != nil {
		serverError(w, "failed to read setting", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key, "value": value})
}

func apiSettingsPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Key) == "" {
		http.Error(w, "missing key", http.StatusBadRequest)
		return
	}
	if err := db.PutSetting(settingsUser(r), strings.TrimSpace(body.Key), body.Value); err != nil {
		serverError(w, "failed to save setting", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiWhoAmI(w http.ResponseWriter, r *http.Request) {
	login, admin, err := currentUser(r)
	if err != nil {
		http.Error(w, "unknown user", http.StatusForbidden)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	localhost := isLocalhost(r)
	perms := []string{"add", "update", "delete"}
	if isLocalUser(r) {
		perms = userPerms
	}
	writeJSON(w, http.StatusOK, map[string]any{"login": login, "hostname": host, "tsMode": localClient != nil, "tsHostname": *tsHostname, "isAdmin": admin || localhost, "localhostAdmin": localhost, "perms": perms})
}

func apiStats(w http.ResponseWriter, r *http.Request) {
	topLinks, err := db.StatsTopLinks(10)
	if err != nil {
		serverError(w, "failed to load top links", err)
		return
	}
	dailyClicks, err := db.StatsDailyClicks(30)
	if err != nil {
		serverError(w, "failed to load daily clicks", err)
		return
	}
	summary, err := db.GetStatsSummary()
	if err != nil {
		serverError(w, "failed to load stats summary", err)
		return
	}
	if topLinks == nil {
		topLinks = []TopLink{}
	}
	if dailyClicks == nil {
		dailyClicks = []DailyCount{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"topLinks":    topLinks,
		"dailyClicks": dailyClicks,
		"summary":     summary,
	})
}

func apiLinxAvatarUpload(w http.ResponseWriter, r *http.Request) {
	if isLocalUser(r) && !hasUserPerm("update") {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	r.ParseMultipartForm(5 << 20) // 5MB max
	file, header, err := r.FormFile("avatar")
	if err != nil {
		http.Error(w, "missing avatar file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "failed to read file", http.StatusInternalServerError)
		return
	}
	mime := header.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/png"
	}
	if err := db.SaveAvatar(id, data, mime); err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiLinxAvatarGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	data, mime, err := db.LoadAvatar(id)
	if err != nil || len(data) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "max-age=3600")
	w.Write(data)
}

var validDocMimes = map[string]bool{
	"text/markdown": true, "text/html": true, "text/plain": true,
}

func apiLinxDocumentUpload(w http.ResponseWriter, r *http.Request) {
	if isLocalUser(r) && !hasUserPerm("update") {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	var payload struct {
		Content string `json:"content"`
		Mime    string `json:"mime"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 5<<20)).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !validDocMimes[payload.Mime] {
		payload.Mime = "text/markdown"
	}
	if err := db.SaveDocument(id, []byte(payload.Content), payload.Mime); err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func apiLinxDocumentGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	data, mime, err := db.LoadDocument(id)
	if err != nil || len(data) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

// expandEnv is the template environment available to destination URL templates.
type expandEnv struct {
	Now   time.Time  // current time (UTC)
	Path  string     // remaining path after short name (e.g. "bar/baz" for /foo/bar/baz)
	user  string     // current user login
	query url.Values // query parameters from the original request
}

var errNoUser = errors.New("no user")

// User returns the current user login, or an error if not logged in.
// Accessible in templates as {{.User}}.
func (e expandEnv) User() (string, error) {
	if e.user == "" {
		return "", errNoUser
	}
	return e.user, nil
}

// Query returns the query parameters from the original request.
// Accessible in templates as {{.Query.key}}.
func (e expandEnv) Query() url.Values {
	return e.query
}

var expandFuncMap = texttemplate.FuncMap{
	"PathEscape":  url.PathEscape,
	"QueryEscape": url.QueryEscape,
	"TrimPrefix":  strings.TrimPrefix,
	"TrimSuffix":  strings.TrimSuffix,
	"ToLower":     strings.ToLower,
	"ToUpper":     strings.ToUpper,
	"Match":       func(pattern, s string) bool { b, _ := regexp.MatchString(pattern, s); return b },
}

// expandLink expands a destination URL template with the given environment.
// If the URL contains no {{...}} template syntax, the remaining path is
// automatically appended (with a / separator if needed).
// Query parameters from the original request are merged into the result.
func expandLink(long string, env expandEnv) (*url.URL, error) {
	if !strings.Contains(long, "{{") {
		if strings.HasSuffix(long, "/") {
			long += "{{.Path}}"
		} else {
			long += "{{with .Path}}/{{.}}{{end}}"
		}
	}
	tmpl, err := texttemplate.New("").Funcs(expandFuncMap).Parse(long)
	if err != nil {
		return nil, err
	}
	buf := new(bytes.Buffer)
	if err := tmpl.Execute(buf, env); err != nil {
		return nil, err
	}

	u, err := url.Parse(buf.String())
	if err != nil {
		return nil, err
	}

	// Merge query parameters from the original request.
	if len(env.query) > 0 {
		q := u.Query()
		for key, values := range env.query {
			for _, v := range values {
				q.Add(key, v)
			}
		}
		u.RawQuery = q.Encode()
	}

	return u, nil
}

func serveRedirect(w http.ResponseWriter, r *http.Request) {
	fullPath := r.PathValue("path")
	short, remainder, _ := strings.Cut(fullPath, "/")

	// Handle + suffix: show detail page instead of redirecting.
	if strings.HasSuffix(short, "+") && remainder == "" {
		detailShort := strings.TrimSuffix(short, "+")
		lnx, err := db.LoadByShortName(detailShort)
		if errors.Is(err, fs.ErrNotExist) {
			if s := strings.TrimRight(detailShort, ".,()[]{}"); s != detailShort {
				detailShort = s
				lnx, err = db.LoadByShortName(detailShort)
			}
		}
		if err != nil {
			serveNotFound(w, r, detailShort)
			return
		}
		if lnx.IsPersonType() {
			serveProfilePage(w, r, lnx)
		} else if lnx.IsDocumentType() {
			serveDocumentPage(w, r, lnx)
		} else {
			serveLinkDetail(w, r, lnx)
		}
		return
	}

	lnx, err := db.LoadByShortName(short)
	if errors.Is(err, fs.ErrNotExist) {
		if s := strings.TrimRight(short, ".,()[]{}"); s != short {
			short = s
			lnx, err = db.LoadByShortName(short)
		}
	}
	if err != nil {
		serveNotFound(w, r, short)
		return
	}
	if lnx.IsPersonType() {
		serveProfilePage(w, r, lnx)
		return
	}
	if lnx.IsDocumentType() {
		serveDocumentPage(w, r, lnx)
		return
	}

	login, _, _ := currentUser(r)
	env := expandEnv{
		Now:   time.Now().UTC(),
		Path:  remainder,
		user:  login,
		query: r.URL.Query(),
	}
	target, err := expandLink(lnx.DestinationURL, env)
	if err != nil {
		if errors.Is(err, errNoUser) {
			http.Error(w, "this link requires a logged-in user", http.StatusUnauthorized)
			return
		}
		http.Error(w, "error expanding link template", http.StatusInternalServerError)
		return
	}

	// Follow local link chains up to the configured depth.
	// Only follow relative URLs (no host) to avoid false matches
	// against external URL paths (e.g. https://google.com/search).
	for i := 0; i < *maxResolveDepth; i++ {
		if target.Host != "" {
			break
		}
		nextShort := extractLocalShortName(target)
		if nextShort == "" {
			break
		}
		next, err := db.LoadByShortName(nextShort)
		if err != nil || next.Type != LinxTypeLink {
			break
		}
		nextTarget, err := expandLink(next.DestinationURL, expandEnv{Now: time.Now().UTC()})
		if err != nil {
			break
		}
		go db.IncrementClick(nextShort)
		target = nextTarget
	}

	go db.IncrementClick(short)
	// Set Location header directly instead of http.Redirect, which cleans the URL.
	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusFound)
}

var notFoundTmpl = template.Must(template.New("notfound").Parse(notFoundPageTemplate))

func serveNotFound(w http.ResponseWriter, r *http.Request, shortName string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	notFoundTmpl.Execute(w, shortName)
}

var notFoundPageTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Not Found - GoLinx</title>
<link rel="icon" type="image/svg+xml" href="/favicon.svg" />
<style>
:root {
  --bar-bg: #1e1e2e; --bar-border: #313244; --bar-text: #cdd6f4;
  --btn-bg: #89b4fa; --btn-text: #1e1e2e; --btn-hover: #74c7ec;
  --panel-bg: #1e1e2e; --panel-border: #313244; --panel-text: #cdd6f4;
  --panel-heading: #89b4fa; --panel-path-text: #a6adc8;
  --panel-btn-bg: #45475a; --panel-btn-text: #cdd6f4;
  --body-bg: #11111b;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body {
  height: 100%; font-family: 'Segoe UI', system-ui, -apple-system, sans-serif;
  background: var(--body-bg); color: var(--panel-text);
}
body { display: flex; flex-direction: column; align-items: center; justify-content: center; padding: 40px 20px; }
.linx-box {
  background: var(--panel-bg); border: 1px solid var(--panel-border);
  border-radius: 12px; padding: 48px 40px; max-width: 480px; width: 100%;
  text-align: center; box-shadow: 0 8px 32px rgba(0,0,0,0.3);
}
.logo { margin-bottom: 24px; }
.logo img { width: 64px; height: 64px; opacity: 0.35; }
.brand {
  font-family: 'Consolas', 'Courier New', monospace; font-size: 1.6rem;
  color: var(--panel-text); margin-bottom: 8px;
}
.brand .accent { color: var(--panel-heading); }
.message { font-size: 1rem; color: var(--panel-text); margin-bottom: 28px; }
.shortname {
  font-family: 'Consolas', 'Courier New', monospace; font-size: 0.9rem;
  color: var(--panel-heading); background: var(--panel-btn-bg);
  padding: 2px 8px; border-radius: 4px;
}
.home-btn {
  display: inline-block; padding: 10px 28px;
  background: var(--btn-bg); color: var(--btn-text);
  border-radius: 8px; text-decoration: none; font-size: 0.9rem; font-weight: 600;
  transition: background 0.15s;
}
.home-btn:hover { background: var(--btn-hover); }
</style>
</head>
<body>
<div class="linx-box">
  <div class="logo"><img src="/logo.svg" alt="GoLinx" width="64" height="64" /></div>
  <div class="brand">Go<span class="accent">Linx</span></div>
  <div class="message">Linx name <span class="shortname">/{{.}}</span> does not exist yet, please go home and add one.</div>
  <a class="home-btn" href="/">Go Home</a>
</div>
</body>
</html>`

var profileTmpl = template.Must(template.New("profile").Parse(profilePageTemplate))

func serveProfilePage(w http.ResponseWriter, r *http.Request, c *Linx) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	profileTmpl.Execute(w, c)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// serverError logs the error to the console and sends an HTTP 500 response.
func serverError(w http.ResponseWriter, msg string, err error) {
	logger.Error(msg, "err", err)
	http.Error(w, msg, http.StatusInternalServerError)
}

// profilePageTemplate is the HTML page shown when visiting /{profile-shortname}.
var profilePageTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.FirstName}} {{.LastName}} - GoLinx</title>
<style>
:root {
  --bar-bg: #1e1e2e; --bar-border: #313244; --bar-text: #cdd6f4;
  --btn-bg: #89b4fa; --btn-text: #1e1e2e; --btn-hover: #74c7ec;
  --panel-bg: #1e1e2e; --panel-border: #313244; --panel-text: #cdd6f4;
  --panel-heading: #89b4fa; --panel-path-text: #a6adc8;
  --panel-btn-bg: #45475a; --panel-btn-text: #cdd6f4;
  --body-bg: #11111b;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body {
  height: 100%; font-family: 'Segoe UI', system-ui, -apple-system, sans-serif;
  background: var(--body-bg); color: var(--panel-text);
}
body { display: flex; flex-direction: column; align-items: center; padding: 40px 20px; }
.back-link {
  position: absolute; top: 16px; left: 20px; color: var(--btn-bg);
  text-decoration: none; font-size: 0.85rem;
}
.back-link:hover { text-decoration: underline; }
.profile-linx {
  background: var(--panel-bg); border: 1px solid var(--panel-border);
  border-radius: 12px; padding: 40px; max-width: 480px; width: 100%;
  text-align: center; box-shadow: 0 8px 32px rgba(0,0,0,0.3);
}
.avatar {
  width: 120px; height: 120px; border-radius: 50%;
  background: var(--panel-btn-bg); margin: 0 auto 20px;
  display: flex; align-items: center; justify-content: center;
  overflow: hidden; border: 3px solid var(--panel-border);
}
.avatar img { width: 100%; height: 100%; object-fit: cover; }
.avatar svg { width: 60px; height: 60px; color: var(--panel-path-text); }
.profile-name {
  font-size: 1.5rem; font-weight: 700; color: var(--panel-heading); margin-bottom: 4px;
}
.profile-short {
  font-size: 0.82rem; color: var(--panel-path-text); margin-bottom: 24px;
}
.info-list { list-style: none; text-align: left; margin-bottom: 24px; }
.info-item {
  display: flex; align-items: center; gap: 12px; padding: 10px 0;
  border-bottom: 1px solid var(--panel-border); font-size: 0.88rem;
}
.info-item:last-child { border-bottom: none; }
.info-item svg { width: 18px; height: 18px; color: var(--panel-path-text); flex-shrink: 0; }
.info-item a { color: var(--btn-bg); text-decoration: none; }
.info-item a:hover { text-decoration: underline; }
.info-item span { color: var(--panel-text); }
.social-links { display: flex; justify-content: center; gap: 12px; }
.social-btn {
  display: flex; align-items: center; gap: 6px; padding: 8px 16px;
  background: var(--panel-btn-bg); color: var(--panel-btn-text);
  border-radius: 8px; text-decoration: none; font-size: 0.82rem;
  transition: background 0.15s;
}
.social-btn:hover { background: var(--btn-bg); color: var(--btn-text); }
.social-btn svg { width: 16px; height: 16px; }
</style>
</head>
<body>
<a class="back-link" href="/">&#8592; GoLinx</a>
<div class="profile-linx">
  <div class="avatar">
    {{if .AvatarMime}}<img src="/api/linx/{{.ID}}/avatar" alt="{{.FirstName}}" />
    {{else}}<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="8" r="4"/><path d="M20 21a8 8 0 0 0-16 0"/></svg>
    {{end}}
  </div>
  <div class="profile-name">{{.FirstName}} {{.LastName}}</div>
  <div class="type-badge" style="display:inline-block;background:var(--btn-bg);color:var(--btn-text);padding:2px 10px;border-radius:10px;font-size:0.75rem;font-weight:600;text-transform:capitalize;margin-bottom:8px">{{.Type}}</div>
  {{if .Title}}<div style="font-size:0.92rem;color:var(--panel-path-text);margin-bottom:4px">{{.Title}}</div>{{end}}
  <div class="profile-short">/{{.ShortName}}</div>
  <ul class="info-list">
    {{if .Email}}<li class="info-item">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="20" height="16" x="2" y="4" rx="2"/><path d="m22 7-8.97 5.7a1.94 1.94 0 0 1-2.06 0L2 7"/></svg>
      <a href="mailto:{{.Email}}">{{.Email}}</a>
    </li>{{end}}
    {{if .Phone}}<li class="info-item">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 16.92v3a2 2 0 0 1-2.18 2 19.79 19.79 0 0 1-8.63-3.07 19.5 19.5 0 0 1-6-6 19.79 19.79 0 0 1-3.07-8.67A2 2 0 0 1 4.11 2h3a2 2 0 0 1 2 1.72c.127.96.361 1.903.7 2.81a2 2 0 0 1-.45 2.11L8.09 9.91a16 16 0 0 0 6 6l1.27-1.27a2 2 0 0 1 2.11-.45c.907.339 1.85.573 2.81.7A2 2 0 0 1 22 16.92z"/></svg>
      <a href="tel:{{.Phone}}">{{.Phone}}</a>
    </li>{{end}}
  </ul>
  {{if or .WebLink .CalLink .XLink .LinkedInLink}}<div class="social-links">
    {{if .WebLink}}<a class="social-btn" href="{{.WebLink}}" target="_blank" rel="noopener">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M2 12h20"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg>
      Website
    </a>{{end}}
    {{if .CalLink}}<a class="social-btn" href="{{.CalLink}}" target="_blank" rel="noopener">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="18" height="18" x="3" y="4" rx="2" ry="2"/><line x1="16" x2="16" y1="2" y2="6"/><line x1="8" x2="8" y1="2" y2="6"/><line x1="3" x2="21" y1="10" y2="10"/></svg>
      Calendar
    </a>{{end}}
    {{if .XLink}}<a class="social-btn" href="{{.XLink}}" target="_blank" rel="noopener">
      <svg viewBox="0 0 24 24" fill="currentColor"><path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z"/></svg>
      X
    </a>{{end}}
    {{if .LinkedInLink}}<a class="social-btn" href="{{.LinkedInLink}}" target="_blank" rel="noopener">
      <svg viewBox="0 0 24 24" fill="currentColor"><path d="M20.447 20.452h-3.554v-5.569c0-1.328-.027-3.037-1.852-3.037-1.853 0-2.136 1.445-2.136 2.939v5.667H9.351V9h3.414v1.561h.046c.477-.9 1.637-1.85 3.37-1.85 3.601 0 4.267 2.37 4.267 5.455v6.286zM5.337 7.433a2.062 2.062 0 0 1-2.063-2.065 2.064 2.064 0 1 1 2.063 2.065zm1.782 13.019H3.555V9h3.564v11.452zM22.225 0H1.771C.792 0 0 .774 0 1.729v20.542C0 23.227.792 24 1.771 24h20.451C23.2 24 24 23.227 24 22.271V1.729C24 .774 23.2 0 22.222 0h.003z"/></svg>
      LinkedIn
    </a>{{end}}
  </div>{{end}}
</div>
</body>
</html>`

type linkDetailBar struct {
	Date   string
	Count  int
	Height int
}

type linkDetailData struct {
	*Linx
	CreatedFormatted string
	DailyBars        []linkDetailBar
	HasClicks        bool
	DateStart        string
	DateMid          string
	DateEnd          string
}

var linkDetailTmpl = template.Must(template.New("linkdetail").Parse(linkDetailPageTemplate))

func serveLinkDetail(w http.ResponseWriter, r *http.Request, c *Linx) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := linkDetailData{
		Linx:             c,
		CreatedFormatted: time.Unix(c.DateCreated, 0).UTC().Format("Jan 2, 2006"),
	}
	// Build 30-day click histogram data server-side
	daily, _ := db.LinkDailyClicks(c.ID, 30)
	dayMap := make(map[string]int, len(daily))
	for _, d := range daily {
		dayMap[d.Date] = d.Count
	}
	now := time.Now()
	max := 1
	var bars []linkDetailBar
	for d := 29; d >= 0; d-- {
		dt := now.AddDate(0, 0, -d)
		key := dt.Format("2006-01-02")
		count := dayMap[key]
		if count > max {
			max = count
		}
		bars = append(bars, linkDetailBar{Date: key, Count: count})
	}
	for i := range bars {
		if bars[i].Count > 0 {
			bars[i].Height = max4(4, bars[i].Count*80/max)
			data.HasClicks = true
		}
	}
	data.DailyBars = bars
	data.DateStart = bars[0].Date[5:]
	data.DateMid = bars[len(bars)/2].Date[5:]
	data.DateEnd = bars[len(bars)-1].Date[5:]
	linkDetailTmpl.Execute(w, data)
}

func max4(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var linkDetailPageTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>/{{.ShortName}} - GoLinx</title>
<link rel="icon" type="image/svg+xml" href="/favicon.svg" />
<style>
:root {
  --bar-bg: #1e1e2e; --bar-border: #313244; --bar-text: #cdd6f4;
  --btn-bg: #89b4fa; --btn-text: #1e1e2e; --btn-hover: #74c7ec;
  --panel-bg: #1e1e2e; --panel-border: #313244; --panel-text: #cdd6f4;
  --panel-heading: #89b4fa; --panel-path-text: #a6adc8;
  --panel-btn-bg: #45475a; --panel-btn-text: #cdd6f4;
  --body-bg: #11111b;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body {
  height: 100%; font-family: 'Segoe UI', system-ui, -apple-system, sans-serif;
  background: var(--body-bg); color: var(--panel-text);
}
body { display: flex; flex-direction: column; align-items: center; padding: 40px 20px; }
.back-link {
  position: absolute; top: 16px; left: 20px; color: var(--btn-bg);
  text-decoration: none; font-size: 0.85rem;
}
.back-link:hover { text-decoration: underline; }
.detail-linx {
  background: var(--panel-bg); border: 1px solid var(--panel-border);
  border-radius: 12px; padding: 40px; max-width: 480px; width: 100%;
  text-align: center; box-shadow: 0 8px 32px rgba(0,0,0,0.3);
}
.link-icon { margin-bottom: 20px; }
.link-icon svg { width: 64px; height: 64px; color: var(--panel-heading); opacity: 0.5; }
.detail-name {
  font-family: 'Consolas', 'Courier New', monospace; font-size: 1.5rem;
  font-weight: 700; color: var(--panel-heading); margin-bottom: 4px;
}
.info-list { list-style: none; text-align: left; margin-bottom: 24px; }
.info-item {
  display: flex; align-items: flex-start; gap: 12px; padding: 10px 0;
  border-bottom: 1px solid var(--panel-border); font-size: 0.88rem;
}
.info-item:last-child { border-bottom: none; }
.info-label { color: var(--panel-path-text); font-size: 0.78rem; text-transform: uppercase; min-width: 80px; flex-shrink: 0; padding-top: 2px; }
.info-value { color: var(--panel-text); word-break: break-all; }
.info-value a { color: var(--btn-bg); text-decoration: none; }
.info-value a:hover { text-decoration: underline; }
.visit-btn {
  display: inline-block; padding: 10px 28px;
  background: var(--btn-bg); color: var(--btn-text);
  border-radius: 8px; text-decoration: none; font-size: 0.9rem; font-weight: 600;
  transition: background 0.15s;
}
.visit-btn:hover { background: var(--btn-hover); }
.detail-chart { margin-bottom: 24px; }
.detail-chart-title { font-size: 0.75rem; color: var(--panel-path-text); text-transform: uppercase; margin-bottom: 8px; }
.detail-bars { display: flex; align-items: flex-end; gap: 2px; height: 84px; }
.detail-bar { flex: 1; background: var(--btn-bg); border-radius: 2px 2px 0 0; min-width: 2px; opacity: 0.7; }
.detail-bar:hover { opacity: 1; }
.detail-bar-labels { display: flex; justify-content: space-between; font-size: 0.6rem; color: var(--panel-path-text); margin-top: 4px; }
.detail-chart-empty { font-size: 0.75rem; color: var(--panel-path-text); text-align: center; padding: 16px 0; }
</style>
</head>
<body>
<a class="back-link" href="/">&#8592; GoLinx</a>
<div class="detail-linx">
  <div class="link-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/></svg></div>
  <div class="detail-name">/{{.ShortName}}</div>
  <ul class="info-list">
    <li class="info-item">
      <span class="info-label">URL</span>
      <span class="info-value"><a href="{{.DestinationURL}}" target="_blank" rel="noopener">{{.DestinationURL}}</a></span>
    </li>
    {{if .Description}}<li class="info-item">
      <span class="info-label">Description</span>
      <span class="info-value">{{.Description}}</span>
    </li>{{end}}
    {{if .Owner}}<li class="info-item">
      <span class="info-label">Owner</span>
      <span class="info-value">{{.Owner}}</span>
    </li>{{end}}
    <li class="info-item">
      <span class="info-label">Clicks</span>
      <span class="info-value">{{.ClickCount}}</span>
    </li>
    {{if .CreatedFormatted}}<li class="info-item">
      <span class="info-label">Created</span>
      <span class="info-value">{{.CreatedFormatted}}</span>
    </li>{{end}}
  </ul>
  <div class="detail-chart">
    <div class="detail-chart-title">Clicks (30 days)</div>
    {{if .HasClicks}}<div class="detail-bars">{{range .DailyBars}}<div class="detail-bar" style="height:{{.Height}}px" title="{{.Date}}: {{.Count}}"></div>{{end}}</div>
    <div class="detail-bar-labels"><span>{{.DateStart}}</span><span>{{.DateMid}}</span><span>{{.DateEnd}}</span></div>{{else}}<div class="detail-chart-empty">No click history yet</div>{{end}}
  </div>
  <a class="visit-btn" href="/{{.ShortName}}">Visit Link &#8594;</a>
</div>
</body>
</html>`

func serveFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(faviconSVG)
}

func serveLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(logoSVG)
}

func serveOpenSearchXML(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host

	w.Header().Set("Content-Type", "application/opensearchdescription+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
  <ShortName>GoLinx</ShortName>
  <Description>Go links</Description>
  <InputEncoding>UTF-8</InputEncoding>
  <Url type="text/html" template="%s/{searchTerms}" />
  <Url type="application/x-suggestions+json" template="%s/api/suggest?q={searchTerms}" />
  <Image height="16" width="16" type="image/svg+xml">%s/favicon.svg</Image>
</OpenSearchDescription>`, base, base, base)
}

func apiSuggest(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeJSON(w, http.StatusOK, []any{query, []string{}, []string{}, []string{}})
		return
	}

	items, err := db.Suggest(query, 8)
	if err != nil {
		serverError(w, "suggest query failed", err)
		return
	}

	names := make([]string, len(items))
	descriptions := make([]string, len(items))
	urls := make([]string, len(items))
	for i, lnx := range items {
		names[i] = lnx.ShortName
		if lnx.IsPersonType() {
			descriptions[i] = lnx.FirstName + " " + lnx.LastName
			if lnx.Title != "" {
				descriptions[i] += " — " + lnx.Title
			}
		} else if lnx.IsDocumentType() {
			descriptions[i] = lnx.Description
		} else {
			descriptions[i] = lnx.DestinationURL
		}
		urls[i] = "/" + lnx.ShortName
	}

	writeJSON(w, http.StatusOK, []any{query, names, descriptions, urls})
}

func serveHelp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, helpPageRendered)
}

// ---------------------------------------------------------------------------
// Deleted Items Page
// ---------------------------------------------------------------------------

type deletedPageData struct {
	Items         []*deletedItem
	RetentionDays int
	KeepForever   bool
}

type deletedItem struct {
	*Linx
	DeletedFormatted string
	ExpiresFormatted string
}

var deletedPageTmpl = template.Must(template.New("deleted").Parse(deletedPageTemplate))

func serveDeleted(w http.ResponseWriter, r *http.Request) {
	items, err := db.LoadDeleted()
	if err != nil {
		serverError(w, "failed to load deleted items", err)
		return
	}
	keepForever := *deleteRetention <= 0
	data := deletedPageData{
		RetentionDays: *deleteRetention,
		KeepForever:   keepForever,
	}
	for _, lnx := range items {
		di := &deletedItem{
			Linx:             lnx,
			DeletedFormatted: time.Unix(lnx.DeletedAt, 0).UTC().Format("Jan 2, 2006 15:04"),
		}
		if !keepForever {
			expires := time.Unix(lnx.DeletedAt, 0).Add(time.Duration(*deleteRetention) * 24 * time.Hour)
			di.ExpiresFormatted = expires.UTC().Format("Jan 2, 2006 15:04")
		}
		data.Items = append(data.Items, di)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	deletedPageTmpl.Execute(w, data)
}

const deletedPageTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Deleted Items - GoLinx</title>
<link rel="icon" type="image/svg+xml" href="/favicon.svg" />
<style>
:root {
  --bar-bg: #1e1e2e;
  --bar-border: #313244;
  --bar-text: #cdd6f4;
  --btn-bg: #89b4fa;
  --btn-text: #1e1e2e;
  --btn-hover: #74c7ec;
  --panel-bg: #1e1e2e;
  --panel-border: #313244;
  --panel-text: #cdd6f4;
  --panel-heading: #89b4fa;
  --panel-path-text: #a6adc8;
  --panel-btn-bg: #45475a;
  --panel-btn-text: #cdd6f4;
  --panel-btn-hover: #585b70;
  --body-bg: #11111b;
  --font: 'Segoe UI', system-ui, -apple-system, sans-serif;
}
*, *::before, *::after { box-sizing: border-box; }
body {
  margin: 0; padding: 24px;
  font-family: var(--font);
  background: var(--body-bg);
  color: var(--panel-text);
  min-height: 100vh;
}
a { color: var(--btn-bg); text-decoration: none; }
a:hover { text-decoration: underline; }
.back-link {
  display: inline-block;
  margin-bottom: 16px;
  color: var(--panel-path-text);
  font-size: 14px;
}
.deleted-page {
  max-width: 900px;
  margin: 0 auto;
}
h1 {
  color: var(--panel-heading);
  font-size: 22px;
  margin: 0 0 8px 0;
}
.retention-note {
  color: var(--panel-path-text);
  font-size: 13px;
  margin: 0 0 20px 0;
}
.empty-note {
  color: var(--panel-path-text);
  font-size: 14px;
  text-align: center;
  padding: 40px 0;
}
table {
  width: 100%;
  border-collapse: collapse;
  background: var(--panel-bg);
  border: 1px solid var(--panel-border);
  border-radius: 8px;
  overflow: hidden;
}
th {
  text-align: left;
  padding: 10px 14px;
  font-size: 12px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  color: var(--panel-path-text);
  border-bottom: 1px solid var(--panel-border);
  background: var(--bar-bg);
}
td {
  padding: 10px 14px;
  font-size: 13px;
  border-bottom: 1px solid var(--panel-border);
  vertical-align: middle;
}
tr:last-child td { border-bottom: none; }
td code {
  color: var(--panel-heading);
  font-size: 13px;
}
.type-cell { text-transform: capitalize; }
.dest-cell {
  max-width: 280px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--panel-path-text);
}
.date-cell {
  white-space: nowrap;
  color: var(--panel-path-text);
  font-size: 12px;
}
.restore-btn {
  padding: 5px 14px;
  background: var(--btn-bg);
  color: var(--btn-text);
  border: none;
  border-radius: 4px;
  cursor: pointer;
  font-size: 12px;
  font-weight: 600;
  white-space: nowrap;
}
.restore-btn:hover { background: var(--btn-hover); }
.toast {
  position: fixed; bottom: 24px; left: 50%;
  transform: translateX(-50%);
  padding: 10px 24px;
  background: var(--panel-bg);
  border: 1px solid var(--panel-border);
  color: var(--panel-text);
  border-radius: 6px;
  font-size: 13px;
  opacity: 0; transition: opacity 0.3s;
  z-index: 999;
}
.toast.show { opacity: 1; }
</style>
</head>
<body>
<a class="back-link" href="/">&#8592; GoLinx</a>
<div class="deleted-page">
<h1>Deleted Items</h1>
{{if .KeepForever}}<p class="retention-note">Deleted items are kept until manually purged.</p>
{{else}}<p class="retention-note">Deleted items are permanently removed after {{.RetentionDays}} days.</p>
{{end}}
{{if .Items}}<table>
<thead>
<tr><th>Short Name</th><th>Type</th><th>Destination / Name</th><th>Deleted</th>{{if not .KeepForever}}<th>Expires</th>{{end}}<th></th></tr>
</thead>
<tbody>
{{range .Items}}<tr id="row-{{.ID}}">
  <td><code>{{.ShortName}}</code></td>
  <td class="type-cell">{{.Type}}</td>
  <td class="dest-cell">{{if .IsPersonType}}{{.FirstName}} {{.LastName}}{{else if .IsDocumentType}}{{.Description}}{{else}}{{.DestinationURL}}{{end}}</td>
  <td class="date-cell">{{.DeletedFormatted}}</td>
  {{if not $.KeepForever}}<td class="date-cell">{{.ExpiresFormatted}}</td>{{end}}
  <td><button class="restore-btn" onclick="restoreItem({{.ID}})">Undelete</button></td>
</tr>
{{end}}</tbody>
</table>
{{else}}<p class="empty-note">No deleted items.</p>
{{end}}
</div>
<div class="toast" id="toast"></div>
<script>
function restoreItem(id) {
  var btn = event.target;
  btn.disabled = true;
  btn.textContent = '...';
  fetch('/api/linx/' + id + '/restore', { method: 'POST' })
    .then(function(r) {
      if (r.ok) {
        var row = document.getElementById('row-' + id);
        row.style.opacity = '0';
        row.style.transition = 'opacity 0.3s';
        setTimeout(function() {
          row.remove();
          if (!document.querySelector('tbody tr')) location.reload();
        }, 300);
        showToast('Restored');
      } else {
        r.text().then(function(msg) {
          showToast('Error: ' + msg);
          btn.disabled = false;
          btn.textContent = 'Undelete';
        });
      }
    });
}
function showToast(msg) {
  var t = document.getElementById('toast');
  t.textContent = msg;
  t.classList.add('show');
  setTimeout(function() { t.classList.remove('show'); }, 2500);
}
</script>
</body>
</html>`

// ---------------------------------------------------------------------------
// TCP Ping
// ---------------------------------------------------------------------------

// validPingHostRe matches a valid hostname or IPv4 address (no brackets).
var validPingHostRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9\-\.]*[a-zA-Z0-9])?$`)

func validatePingHost(raw string) error {
	if raw == "" {
		return fmt.Errorf("host is required")
	}
	h, p, err := net.SplitHostPort(raw)
	if err != nil {
		h = raw // no port
	}
	if len(h) > 253 {
		return fmt.Errorf("hostname too long")
	}
	if p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid port %q", p)
		}
	}
	// Allow valid IPs (v4/v6) or hostnames matching the regex.
	if net.ParseIP(h) == nil && !validPingHostRe.MatchString(h) {
		return fmt.Errorf("invalid hostname %q", h)
	}
	return nil
}

func parsePingTarget(raw string) (host, port string) {
	h, p, err := net.SplitHostPort(raw)
	if err != nil {
		return raw, "80"
	}
	if p == "" {
		p = "80"
	}
	return h, p
}

func tcpPing(ctx context.Context, addr string, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	rtt := time.Since(start)
	if err != nil {
		return 0, err
	}
	conn.Close()
	return rtt, nil
}

func sendSSE(w http.ResponseWriter, flusher http.Flusher, event, data string) {
	fmt.Fprintf(w, "event: %s\n", event)
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
	flusher.Flush()
}

func servePing(w http.ResponseWriter, r *http.Request) {
	host := r.PathValue("host")
	if err := validatePingHost(host); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pingHost, pingPort := parsePingTarget(host)
	if r.URL.Query().Get("stream") == "1" {
		servePingSSE(w, r, pingHost, pingPort)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	display := net.JoinHostPort(pingHost, pingPort)
	terminalPageTmpl.Execute(w, terminalPageData{
		Title:  "Ping \u2014 " + display,
		Stream: "/.ping/" + host + "?stream=1",
	})
}

func servePingSSE(w http.ResponseWriter, r *http.Request, host, port string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()
	target := net.JoinHostPort(host, port)

	const (
		pingCount    = 5
		pingInterval = 1 * time.Second
		pingTimeout  = 5 * time.Second
	)

	sendSSE(w, flusher, "status", fmt.Sprintf("PING %s (tcp)", target))

	var sent, received int
	var minRTT, maxRTT, totalRTT time.Duration

	for i := 0; i < pingCount; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		sent++
		rtt, err := tcpPing(ctx, target, pingTimeout)
		if err != nil {
			sendSSE(w, flusher, "info", fmt.Sprintf(
				"seq=%d  %s  err=%s", i+1, target, err.Error()))
		} else {
			received++
			if minRTT == 0 || rtt < minRTT {
				minRTT = rtt
			}
			if rtt > maxRTT {
				maxRTT = rtt
			}
			totalRTT += rtt
			sendSSE(w, flusher, "info", fmt.Sprintf(
				"seq=%d  %s  time=%.2fms", i+1, target,
				float64(rtt.Microseconds())/1000.0))
		}

		if i < pingCount-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(pingInterval):
			}
		}
	}

	loss := float64(sent-received) / float64(sent) * 100
	var avgRTT time.Duration
	if received > 0 {
		avgRTT = totalRTT / time.Duration(received)
	}
	sendSSE(w, flusher, "summary", fmt.Sprintf(
		"--- %s ping statistics ---\n%d packets sent, %d received, %.0f%% loss\nrtt min/avg/max = %.2f/%.2f/%.2fms",
		target, sent, received, loss,
		float64(minRTT.Microseconds())/1000.0,
		float64(avgRTT.Microseconds())/1000.0,
		float64(maxRTT.Microseconds())/1000.0))

	sendSSE(w, flusher, "done", "complete")
}

func serveWhoAmI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("stream") == "1" {
		serveWhoAmISSE(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	terminalPageTmpl.Execute(w, terminalPageData{
		Title:  "Who Am I",
		Stream: "/.whoami?stream=1",
	})
}

func serveWhoAmISSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	if localClient == nil {
		identity, _, _ := currentUser(r)
		sendSSE(w, flusher, "status", "WHOAMI (local mode)")
		sendSSE(w, flusher, "info", "Identity:  "+identity)
		sendSSE(w, flusher, "info", "Remote:    "+r.RemoteAddr)
		if isLocalhost(r) {
			sendSSE(w, flusher, "info", "Admin:     yes (localhost)")
		} else {
			sendSSE(w, flusher, "info", "Perms:     "+strings.Join(userPerms, ", "))
		}
		sendSSE(w, flusher, "info", "")
		sendSSE(w, flusher, "info", "Full WhoIs requires a Tailscale listener.")
		sendSSE(w, flusher, "done", "complete")
		return
	}

	whois, err := localClient.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		sendSSE(w, flusher, "status", "WHOAMI")
		sendSSE(w, flusher, "info", "err="+err.Error())
		sendSSE(w, flusher, "done", "complete")
		return
	}

	sendSSE(w, flusher, "status", "WHOAMI (tailscale)")

	if whois.Node != nil {
		n := whois.Node
		sendSSE(w, flusher, "info", "Machine:")
		sendSSE(w, flusher, "info", "  Name:       "+strings.TrimSuffix(n.Name, "."))
		sendSSE(w, flusher, "info", "  ID:         "+string(n.StableID))
		if len(n.Addresses) > 0 {
			addrs := make([]string, len(n.Addresses))
			for i, a := range n.Addresses {
				addrs[i] = a.String()
			}
			sendSSE(w, flusher, "info", "  Addresses:  "+strings.Join(addrs, ", "))
		}
		if n.IsTagged() {
			sendSSE(w, flusher, "info", "  Tags:       "+strings.Join(n.Tags, ", "))
		}
		if hi := n.Hostinfo; hi.Valid() {
			if os := hi.OS(); os != "" {
				sendSSE(w, flusher, "info", "  OS:         "+os)
			}
			if hostname := hi.Hostname(); hostname != "" {
				sendSSE(w, flusher, "info", "  Hostname:   "+hostname)
			}
		}
	}

	if whois.UserProfile != nil && !whois.Node.IsTagged() {
		p := whois.UserProfile
		sendSSE(w, flusher, "info", "")
		sendSSE(w, flusher, "info", "User:")
		sendSSE(w, flusher, "info", "  Login:      "+p.LoginName)
		if p.DisplayName != "" {
			sendSSE(w, flusher, "info", "  Name:       "+p.DisplayName)
		}
		sendSSE(w, flusher, "info", "  ID:         "+fmt.Sprintf("%d", p.ID))
		if p.ProfilePicURL != "" {
			sendSSE(w, flusher, "info", "  Avatar:     "+p.ProfilePicURL)
		}
	}

	if len(whois.CapMap) > 0 {
		sendSSE(w, flusher, "info", "")
		sendSSE(w, flusher, "info", "Capabilities:")
		for cap := range whois.CapMap {
			sendSSE(w, flusher, "info", "  - "+string(cap))
		}
	}

	sendSSE(w, flusher, "done", "complete")
}

type terminalPageData struct {
	Title  string
	Stream string
}

var terminalPageTmpl = template.Must(template.New("terminal").Parse(terminalPageTemplate))

const terminalPageTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} - GoLinx</title>
<link rel="icon" type="image/svg+xml" href="/favicon.svg" />
<style>
:root {
  --bar-bg: #1e1e2e; --bar-border: #313244; --bar-text: #cdd6f4;
  --btn-bg: #89b4fa; --btn-text: #1e1e2e;
  --panel-bg: #1e1e2e; --panel-border: #313244; --panel-text: #cdd6f4;
  --panel-heading: #89b4fa; --panel-path-text: #a6adc8;
  --panel-btn-bg: #45475a; --panel-btn-text: #cdd6f4;
  --body-bg: #11111b;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body {
  height: 100%; font-family: 'Segoe UI', system-ui, -apple-system, sans-serif;
  background: var(--body-bg); color: var(--panel-text);
}
body { display: flex; flex-direction: column; align-items: stretch; padding: 40px; }
.back-link {
  color: var(--btn-bg); text-decoration: none; font-size: 0.85rem; margin-bottom: 16px;
}
.back-link:hover { text-decoration: underline; }
.terminal-page {
  background: var(--panel-bg); border: 1px solid var(--panel-border);
  border-radius: 12px; padding: 40px; width: 100%;
  box-shadow: 0 8px 32px rgba(0,0,0,0.3);
}
h1 { font-size: 1.5rem; color: var(--panel-heading); margin-bottom: 20px; }
.console {
  background: var(--body-bg); border: 1px solid var(--panel-border);
  border-radius: 8px; padding: 16px 20px; min-height: 200px;
  font-family: 'Consolas', 'Courier New', monospace; font-size: 0.85rem;
  line-height: 1.7; overflow-x: auto; white-space: pre-wrap;
  color: var(--panel-text);
}
.console .ok { color: #a6e3a1; }
.console .fail { color: #f38ba8; }
.console .info { color: var(--panel-path-text); }
.console .sum { color: var(--panel-heading); }
.console .cur {
  display: inline-block; width: 8px; height: 14px;
  background: var(--panel-heading); animation: blink 1s step-end infinite;
  vertical-align: text-bottom; margin-left: 2px;
}
@keyframes blink { 50% { opacity: 0; } }
</style>
</head>
<body>
<a class="back-link" href="/">&#8592; GoLinx</a>
<div class="terminal-page">
  <h1>{{.Title}}</h1>
  <div class="console" id="con"><span class="info">Connecting...</span><span class="cur" id="cur"></span></div>
</div>
<script>
(function() {
  var con = document.getElementById('con');
  var cur = document.getElementById('cur');
  function add(text, cls) {
    var s = document.createElement('span');
    s.className = cls || '';
    s.textContent = text + '\n';
    con.insertBefore(s, cur);
    con.scrollTop = con.scrollHeight;
  }
  function clearInit() {
    var el = con.querySelector('.info');
    if (el) el.remove();
  }
  var es = new EventSource('{{.Stream}}');
  es.addEventListener('status', function(e) {
    clearInit();
    add(e.data, 'info');
    add('', '');
  });
  es.addEventListener('info', function(e) {
    add(e.data, e.data.indexOf('err=') !== -1 ? 'fail' : 'ok');
  });
  es.addEventListener('summary', function(e) {
    add('', '');
    e.data.split('\n').forEach(function(l) { add(l, 'sum'); });
  });
  es.addEventListener('done', function() {
    es.close();
    if (cur) cur.remove();
  });
  es.onerror = function() {
    clearInit();
    add('Connection lost.', 'fail');
    es.close();
    if (cur) cur.remove();
  };
})();
</script>
</body>
</html>`

func apiDBGet(w http.ResponseWriter, r *http.Request) {
	items, err := db.LoadAll("")
	if err != nil {
		serverError(w, "failed to load linx", err)
		return
	}
	if items == nil {
		items = []*Linx{}
	}
	writeJSON(w, http.StatusOK, items)
}

func apiDBPut(w http.ResponseWriter, r *http.Request) {
	var items []Linx
	if err := json.NewDecoder(r.Body).Decode(&items); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	var added, skipped int
	for _, c := range items {
		if c.ShortName == "" {
			continue
		}
		if _, err := db.LoadByShortName(c.ShortName); err == nil {
			skipped++
			continue
		}
		c.ID = 0
		if _, err := db.Save(&c); err != nil {
			continue
		}
		added++
	}
	writeJSON(w, http.StatusOK, map[string]int{"added": added, "skipped": skipped})
}

// ---------------------------------------------------------------------------
// Document reader page
// ---------------------------------------------------------------------------

var documentTmpl = template.Must(template.New("document").Parse(documentPageTemplate))

func serveDocumentPage(w http.ResponseWriter, r *http.Request, c *Linx) {
	data, mime, err := db.LoadDocument(c.ID)
	if err != nil || len(data) == 0 {
		http.Error(w, "document content not found", http.StatusNotFound)
		return
	}

	var rendered string
	switch mime {
	case "text/markdown":
		md := goldmark.New(goldmark.WithExtensions(extension.Table))
		var buf bytes.Buffer
		md.Convert(data, &buf)
		p := bluemonday.UGCPolicy()
		rendered = p.Sanitize(buf.String())
	case "text/html":
		p := bluemonday.UGCPolicy()
		rendered = p.Sanitize(string(data))
	default: // text/plain or unknown
		rendered = `<pre class="doc-pre">` + template.HTMLEscapeString(string(data)) + `</pre>`
	}

	go db.IncrementClick(c.ShortName)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	documentTmpl.Execute(w, struct {
		*Linx
		RenderedHTML template.HTML
	}{c, template.HTML(rendered)})
}

var documentPageTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Description}} - GoLinx</title>
<link rel="icon" type="image/svg+xml" href="/favicon.svg" />
<style>
:root {
  --bar-bg: #1e1e2e; --bar-border: #313244; --bar-text: #cdd6f4;
  --btn-bg: #89b4fa; --btn-text: #1e1e2e;
  --panel-bg: #1e1e2e; --panel-border: #313244; --panel-text: #cdd6f4;
  --panel-heading: #89b4fa; --panel-path-text: #a6adc8;
  --panel-btn-bg: #45475a; --panel-btn-text: #cdd6f4;
  --body-bg: #11111b;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body {
  height: 100%; font-family: 'Segoe UI', system-ui, -apple-system, sans-serif;
  background: var(--body-bg); color: var(--panel-text);
}
body { display: flex; flex-direction: column; align-items: center; padding: 40px 20px; }
.back-link {
  position: absolute; top: 16px; left: 20px; color: var(--btn-bg);
  text-decoration: none; font-size: 0.85rem;
}
.back-link:hover { text-decoration: underline; }
.doc-page {
  background: var(--panel-bg); border: 1px solid var(--panel-border);
  border-radius: 12px; padding: 40px; max-width: 750px; width: 100%;
  box-shadow: 0 8px 32px rgba(0,0,0,0.3);
}
.doc-header { margin-bottom: 24px; padding-bottom: 16px; border-bottom: 1px solid var(--panel-border); }
.doc-header h1 { font-size: 1.5rem; color: var(--panel-heading); margin-bottom: 8px; }
.doc-meta { display: flex; gap: 16px; font-size: 0.8rem; color: var(--panel-path-text); }
h1 { font-size: 1.5rem; color: var(--panel-heading); margin: 24px 0 12px; }
h2 {
  font-size: 1.15rem; color: var(--panel-heading); margin: 24px 0 10px;
  padding-bottom: 4px; border-bottom: 1px solid var(--panel-border);
}
h3 { font-size: 1rem; color: var(--panel-heading); margin: 20px 0 8px; }
h4, h5, h6 { font-size: 0.9rem; color: var(--panel-heading); margin: 16px 0 6px; }
p, li { font-size: 0.88rem; line-height: 1.7; color: var(--panel-text); }
p { margin-bottom: 10px; }
ul, ol { padding-left: 24px; margin-bottom: 10px; }
li { margin-bottom: 4px; }
a { color: var(--btn-bg); }
a:hover { text-decoration: underline; }
blockquote {
  border-left: 3px solid var(--panel-border); padding: 8px 16px; margin: 10px 0;
  color: var(--panel-path-text); font-style: italic;
}
code {
  background: var(--panel-btn-bg); color: var(--panel-btn-text);
  padding: 1px 6px; border-radius: 3px; font-size: 0.82rem;
  font-family: 'Consolas', 'Courier New', monospace;
}
pre {
  background: var(--panel-btn-bg); border-radius: 6px; padding: 12px 16px;
  margin: 10px 0 14px; overflow-x: auto;
}
pre code {
  background: none; padding: 0; border-radius: 0; font-size: 0.82rem;
  color: var(--panel-btn-text);
}
.doc-pre {
  background: var(--panel-btn-bg); border-radius: 6px; padding: 16px 20px;
  overflow-x: auto; font-family: 'Consolas', 'Courier New', monospace;
  font-size: 0.85rem; line-height: 1.6; white-space: pre-wrap;
  word-break: break-word; color: var(--panel-btn-text);
}
table {
  width: 100%; border-collapse: collapse; margin: 10px 0 14px;
  font-size: 0.84rem;
}
th, td {
  text-align: left; padding: 6px 10px;
  border-bottom: 1px solid var(--panel-border);
}
th { color: var(--panel-heading); font-weight: 600; font-size: 0.78rem; text-transform: uppercase; letter-spacing: 0.5px; }
td { color: var(--panel-text); }
td code { font-size: 0.8rem; }
img { max-width: 100%; height: auto; border-radius: 6px; margin: 8px 0; }
hr { border: none; border-top: 1px solid var(--panel-border); margin: 20px 0; }
</style>
</head>
<body>
<a class="back-link" href="/">&#8592; GoLinx</a>
<div class="doc-page">
  <div class="doc-header">
    <h1>{{.Description}}</h1>
    <div class="doc-meta">
      <span>/{{.ShortName}}</span>
      <span>{{.ClickCount}} views</span>
    </div>
  </div>
  <div class="doc-content">{{.RenderedHTML}}</div>
</div>
</body>
</html>`

func serveExport(w http.ResponseWriter, r *http.Request) {
	items, err := db.LoadAll("")
	if err != nil {
		serverError(w, "failed to export linx", err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="links.json"`)
	json.NewEncoder(w).Encode(items)
}

var helpPageRendered string
var destURLHelpHTML string

func init() {
	md := goldmark.New(goldmark.WithExtensions(extension.Table))
	var buf bytes.Buffer
	if err := md.Convert(helpMD, &buf); err != nil {
		panic("render help.md: " + err.Error())
	}
	versionDiv := `<div style="text-align:center;padding:20px 0 0;color:#6c7086;font-size:0.8rem;border-top:1px solid var(--panel-border);margin-top:24px;">GoLinx ` + Version + `</div>`
	helpPageRendered = helpPagePrefix + buf.String() + versionDiv + helpPageSuffix

	buf.Reset()
	if err := md.Convert(destURLHelpMD, &buf); err != nil {
		panic("render dest-url-help.md: " + err.Error())
	}
	destURLHelpHTML = buf.String()
	pageTemplate = strings.Replace(pageTemplate, "<!--DEST_URL_HELP-->", destURLHelpHTML, 1)
}

const helpPagePrefix = `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Help - GoLinx</title>
<link rel="icon" type="image/svg+xml" href="/favicon.svg" />
<style>
:root {
  --bar-bg: #1e1e2e; --bar-border: #313244; --bar-text: #cdd6f4;
  --btn-bg: #89b4fa; --btn-text: #1e1e2e;
  --panel-bg: #1e1e2e; --panel-border: #313244; --panel-text: #cdd6f4;
  --panel-heading: #89b4fa; --panel-path-text: #a6adc8;
  --panel-btn-bg: #45475a; --panel-btn-text: #cdd6f4;
  --body-bg: #11111b;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body {
  height: 100%; font-family: 'Segoe UI', system-ui, -apple-system, sans-serif;
  background: var(--body-bg); color: var(--panel-text);
}
body { display: flex; flex-direction: column; align-items: center; padding: 40px 20px; }
.back-link {
  position: absolute; top: 16px; left: 20px; color: var(--btn-bg);
  text-decoration: none; font-size: 0.85rem;
}
.back-link:hover { text-decoration: underline; }
.help-page {
  background: var(--panel-bg); border: 1px solid var(--panel-border);
  border-radius: 12px; padding: 40px; max-width: 680px; width: 100%;
  box-shadow: 0 8px 32px rgba(0,0,0,0.3);
}
h1 { font-size: 1.5rem; color: var(--panel-heading); margin-bottom: 24px; }
h2 {
  font-size: 1rem; color: var(--panel-heading); margin: 24px 0 10px;
  padding-bottom: 4px; border-bottom: 1px solid var(--panel-border);
}
h2:first-of-type { margin-top: 0; }
p, li { font-size: 0.88rem; line-height: 1.6; color: var(--panel-text); }
p { margin-bottom: 8px; }
ul { padding-left: 20px; margin-bottom: 8px; }
li { margin-bottom: 4px; }
code {
  background: var(--panel-btn-bg); color: var(--panel-btn-text);
  padding: 1px 6px; border-radius: 3px; font-size: 0.82rem;
  font-family: 'Consolas', 'Courier New', monospace;
}
pre {
  background: var(--panel-btn-bg); border-radius: 6px; padding: 12px 16px;
  margin: 8px 0 12px; overflow-x: auto;
}
pre code {
  background: none; padding: 0; border-radius: 0; font-size: 0.82rem;
  color: var(--panel-btn-text);
}
table {
  width: 100%; border-collapse: collapse; margin: 8px 0 12px;
  font-size: 0.84rem;
}
th, td {
  text-align: left; padding: 6px 10px;
  border-bottom: 1px solid var(--panel-border);
}
th { color: var(--panel-heading); font-weight: 600; font-size: 0.78rem; text-transform: uppercase; letter-spacing: 0.5px; }
td { color: var(--panel-text); }
td code { font-size: 0.8rem; }
</style>
</head>
<body>
<a class="back-link" href="/">&#8592; GoLinx</a>
<div class="help-page">
`

const helpPageSuffix = `
</div>
</body>
</html>`

// pageTemplate is the complete embedded SPA served at /.
var pageTemplate = `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>GoLinx</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Share+Tech+Mono&display=swap">
<link rel="icon" type="image/svg+xml" href="/favicon.svg" />
<link rel="search" type="application/opensearchdescription+xml" href="/opensearch.xml" title="GoLinx" />
<style>
:root {
  --bar-bg: #1e1e2e;
  --bar-border: #313244;
  --bar-text: #cdd6f4;
  --input-bg: #313244;
  --input-border: #45475a;
  --input-text: #cdd6f4;
  --input-placeholder: #6c7086;
  --btn-bg: #89b4fa;
  --btn-text: #1e1e2e;
  --btn-hover: #74c7ec;
  --btn-danger-bg: #f38ba8;
  --btn-danger-hover: #eba0ac;
  --panel-bg: #1e1e2e;
  --panel-border: #313244;
  --panel-text: #cdd6f4;
  --panel-heading: #89b4fa;
  --panel-path-text: #a6adc8;
  --panel-path-border: #313244;
  --panel-hover: #313244;
  --panel-btn-bg: #45475a;
  --panel-btn-text: #cdd6f4;
  --panel-btn-hover: #585b70;
  --check-accent: #89b4fa;
  --body-bg: #11111b;
  --font: 'Segoe UI', system-ui, -apple-system, sans-serif;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
.hidden { display: none !important; }
body[data-square] * { border-radius: 0 !important; }
html, body {
  height: 100%; font-family: var(--font);
  background: var(--body-bg); color: var(--bar-text); overflow: hidden;
}
body { display: flex; flex-direction: column; height: 100vh; }

#headerbar {
  display: flex; align-items: center; gap: 10px; padding: 8px 16px;
  background: var(--bar-bg); border-bottom: 1px solid var(--bar-border);
  flex-shrink: 0; z-index: 10;
}
.brand {
  font-weight: 700; font-size: 1.3rem; color: var(--bar-text);
  letter-spacing: 0.5px; margin-right: auto;
  display: flex; align-items: center; gap: 8px;
}
.brand .accent { color: var(--panel-heading); }
.brand svg { width: 18px; height: 18px; flex-shrink: 0; }
#themeSelect {
  background: var(--input-bg); border: 1px solid var(--input-border);
  border-radius: 4px; color: var(--input-text); padding: 4px 8px;
  font-size: 0.8rem; outline: none; cursor: pointer; font-family: inherit;
}
#themeSelect option { background: var(--input-bg); color: var(--input-text); }
.admin-toggle {
  display: flex; align-items: center; gap: 6px; cursor: pointer;
  font-size: 0.78rem; color: var(--panel-path-text); margin-left: 8px; user-select: none;
}
.admin-toggle.hidden { display: none; }
.admin-toggle input { display: none; }
.admin-slider {
  width: 32px; height: 18px; background: var(--input-border); border-radius: 9px;
  position: relative; transition: background 0.2s;
}
.admin-slider::after {
  content: ''; position: absolute; top: 2px; left: 2px;
  width: 14px; height: 14px; background: var(--panel-bg); border-radius: 50%;
  transition: transform 0.2s;
}
.admin-toggle input:checked + .admin-slider { background: var(--btn-bg); }
.admin-toggle input:checked + .admin-slider::after { transform: translateX(14px); }
.admin-label { font-weight: 600; }

#main {
  flex: 1; display: flex; flex-direction: column; overflow: hidden;
  padding: 20px 24px 0;
}
#search-area {
  display: flex; gap: 12px; margin-bottom: 16px; align-items: center;
  justify-content: center; flex-shrink: 0; max-width: 700px;
  margin-left: auto; margin-right: auto; width: 100%;
}
#searchInput {
  flex: 1; padding: 10px 16px; font-size: 1rem;
  background: var(--input-bg); border: 1px solid var(--input-border);
  border-radius: 8px; color: var(--input-text); outline: none;
  font-family: inherit; transition: border-color 0.15s;
}
#searchInput::placeholder { color: var(--input-placeholder); }
#searchInput:focus { border-color: var(--btn-bg); }
#addBtn {
  width: 44px; height: 44px; border-radius: 50%; border: none;
  background: var(--btn-bg); color: var(--btn-text);
  font-size: 1.6rem; font-weight: 700; cursor: pointer;
  display: flex; align-items: center; justify-content: center;
  transition: background 0.15s; flex-shrink: 0;
}
#addBtn:hover { background: var(--btn-hover); }
#toolbar {
  display: flex; justify-content: space-between; align-items: center;
  margin-bottom: 8px; flex-shrink: 0;
}
#sort-btns, #view-btns { display: flex; gap: 4px; }
.sort-btn {
  padding: 4px 12px; border-radius: 6px; border: none;
  background: transparent; color: var(--panel-path-text);
  cursor: pointer; font-size: 0.78rem; font-family: inherit;
  transition: color 0.15s, background 0.15s;
}
.sort-btn:hover { color: var(--btn-bg); }
.sort-btn.sort-active { background: var(--panel-btn-bg); color: var(--btn-bg); }
.view-btn {
  width: 36px; height: 36px; border-radius: 6px; border: none;
  background: transparent; color: var(--panel-path-text);
  cursor: pointer; display: flex; align-items: center; justify-content: center;
  transition: color 0.15s, background 0.15s; padding: 0;
}
.view-btn:hover { color: var(--btn-bg); }
.view-btn.view-active { background: var(--panel-btn-bg); color: var(--btn-bg); }

#grid-container {
  flex: 1; overflow: auto; border-radius: 8px;
  border: 1px solid var(--panel-border); background: var(--panel-bg);
}
#link-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: 12px; padding: 16px; min-width: 600px;
}
#no-results {
  text-align: center; padding: 60px 20px; color: var(--panel-path-text);
  font-size: 0.95rem;
}

.linx-item {
  background: var(--bar-bg); border: 1px solid var(--panel-border);
  border-radius: 6px; padding: 14px 16px;
  transition: border-color 0.15s, box-shadow 0.15s;
  cursor: default; display: flex; flex-direction: column; gap: 6px;
}
.linx-item:hover {
  border-color: var(--btn-bg);
  box-shadow: 0 2px 12px rgba(0,0,0,0.25);
}
.linx-shortname {
  font-weight: 700; font-size: 0.95rem; color: var(--panel-heading);
  align-self: flex-start;
}
.linx-item:focus { outline: none; border-color: var(--btn-bg); border-width: 2px; box-shadow: 0 2px 12px rgba(0,0,0,0.25); }
.linx-url {
  font-size: 0.78rem; color: var(--panel-path-text);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.linx-desc {
  font-size: 0.82rem; color: var(--panel-text); line-height: 1.4;
}
.linx-meta {
  display: flex; justify-content: space-between; align-items: center;
  font-size: 0.72rem; color: var(--panel-path-text); margin-top: auto;
  padding-top: 6px; border-top: 1px solid var(--panel-border);
}
.linx-owner {
  background: var(--panel-btn-bg); color: var(--panel-btn-text);
  padding: 2px 8px; border-radius: 10px; font-size: 0.7rem;
}
.linx-clicks { display: flex; align-items: center; gap: 4px; }

/* Profile linx */
.linx-item.profile-linx { border-left: 3px solid var(--btn-bg); }
.profile-linx-body { display: flex; gap: 12px; align-items: flex-start; }
.profile-avatar {
  width: 44px; height: 44px; border-radius: 50%; flex-shrink: 0;
  background: var(--panel-btn-bg); display: flex; align-items: center;
  justify-content: center; overflow: hidden;
}
.profile-avatar img { width: 100%; height: 100%; object-fit: cover; }
.profile-avatar svg { width: 24px; height: 24px; color: var(--panel-path-text); }
.profile-info { flex: 1; min-width: 0; }
.profile-name { font-weight: 700; font-size: 0.95rem; color: var(--panel-heading); }
.profile-email { font-size: 0.78rem; color: var(--panel-path-text); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.profile-short { font-size: 0.75rem; color: var(--panel-path-text); }
.linx-item.doc-linx { border-left: 3px solid #a6e3a1; }
.doc-linx-body { display: flex; gap: 12px; align-items: flex-start; }
.doc-icon {
  width: 40px; height: 40px; border-radius: 8px; flex-shrink: 0;
  background: var(--panel-btn-bg); display: flex; align-items: center;
  justify-content: center;
}
.doc-icon svg { width: 22px; height: 22px; color: #a6e3a1; }
.doc-info { flex: 1; min-width: 0; }
.doc-short { font-size: 0.75rem; color: var(--panel-path-text); }
.linx-badge {
  background: var(--btn-bg); color: var(--btn-text); padding: 2px 8px;
  border-radius: 10px; font-size: 0.68rem; font-weight: 600;
}
.linx-tags { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 4px; }
.tag-badge {
  font-size: 0.65rem; padding: 1px 6px; border-radius: 8px;
  background: var(--input-border); color: var(--panel-text); opacity: 0.8;
}
.tag-autocomplete {
  position: absolute; left: 0; right: 0; top: 100%; z-index: 20;
  background: var(--panel-bg); border: 1px solid var(--input-border);
  border-radius: 6px; box-shadow: 0 4px 12px rgba(0,0,0,0.3);
  max-height: 160px; overflow-y: auto;
}
.tag-ac-item {
  padding: 5px 10px; font-size: 0.8rem; cursor: pointer;
  color: var(--panel-text);
}
.tag-ac-item:hover { background: var(--input-border); }

/* List view mode */
#link-grid.list-mode { grid-template-columns: 1fr; min-width: 0; gap: 4px; }
#link-grid.list-mode .linx-item {
  flex-direction: row; align-items: center; gap: 16px;
  padding: 8px 16px; border-radius: 4px;
}
#link-grid.list-mode .linx-shortname {
  min-width: 120px; max-width: 160px; white-space: nowrap;
  overflow: hidden; text-overflow: ellipsis; flex-shrink: 0;
}
#link-grid.list-mode .linx-url {
  flex: 1; min-width: 0;
}
#link-grid.list-mode .linx-desc {
  flex: 1; min-width: 0; white-space: nowrap;
  overflow: hidden; text-overflow: ellipsis;
}
#link-grid.list-mode .linx-meta {
  border-top: none; margin-top: 0; padding-top: 0;
  min-width: 140px; flex-shrink: 0; justify-content: flex-end; gap: 12px;
}

#statusbar {
  padding: 3px 16px; font-size: 0.72rem; color: var(--panel-path-text);
  background: var(--bar-bg); border-top: 1px solid var(--bar-border);
  display: flex; justify-content: space-between; align-items: center; flex-shrink: 0;
}
#statusbar-left { display: flex; align-items: center; gap: 8px; }
#gearBtn {
  background: none; border: none; color: var(--panel-path-text);
  cursor: pointer; padding: 0; display: flex; align-items: center;
  transition: color 0.15s;
}
#gearBtn:hover { color: var(--panel-text); }
#gearBtn svg { width: 14px; height: 14px; }
#gear-menu {
  display: none; position: fixed; z-index: 1000;
  background: var(--panel-bg); border: 1px solid var(--panel-border);
  border-radius: 4px; padding: 4px 0; min-width: 140px;
  box-shadow: 0 4px 12px rgba(0,0,0,0.4);
}
#gear-menu.visible { display: block; }

/* Context menu */
#ctx-menu {
  display: none; position: fixed; z-index: 1000;
  background: var(--panel-bg); border: 1px solid var(--panel-border);
  border-radius: 4px; padding: 4px 0; min-width: 140px;
  box-shadow: 0 4px 12px rgba(0,0,0,0.4);
}
#ctx-menu.visible { display: block; }
.ctx-item {
  padding: 6px 16px; font-size: 0.82rem; color: var(--panel-text);
  cursor: pointer; transition: background 0.1s;
}
.ctx-item:hover { background: var(--panel-hover); }
.ctx-item.danger { color: var(--btn-danger-bg); }
.ctx-item.danger:hover { color: var(--btn-danger-hover); }
.ctx-separator { height: 1px; background: var(--panel-border); margin: 4px 0; }

/* Modal overlay */
.modal-overlay {
  position: fixed; inset: 0; z-index: 9999;
  background: rgba(0,0,0,0.45); display: flex;
  align-items: center; justify-content: center;
}
.modal-overlay.hidden { display: none; }
.modal-box {
  background: var(--panel-bg); border: 1px solid var(--panel-border);
  border-radius: 8px; min-width: 380px; max-width: 500px; width: 90%;
  box-shadow: 0 8px 32px rgba(0,0,0,0.5); overflow: hidden;
}
.modal-title {
  padding: 12px 16px; font-size: 0.88rem; font-weight: 600;
  color: var(--panel-heading); background: var(--bar-bg);
  border-bottom: 1px solid var(--panel-border);
}
.modal-body { padding: 16px; font-size: 0.82rem; color: var(--panel-text); }
.modal-actions {
  display: flex; justify-content: flex-end; gap: 8px;
  padding: 12px 16px; border-top: 1px solid var(--panel-border);
}
.modal-actions button {
  padding: 6px 16px; border: none; border-radius: 4px;
  font-size: 0.82rem; font-weight: 600; cursor: pointer;
  font-family: inherit; transition: filter 0.1s;
}
.mbtn-primary { background: var(--btn-bg); color: var(--btn-text); }
.mbtn-primary:hover { filter: brightness(1.1); }
.mbtn-danger { background: var(--btn-danger-bg); color: #fff; }
.mbtn-danger:hover { background: var(--btn-danger-hover); }
.mbtn-cancel { background: var(--panel-btn-bg); color: var(--panel-btn-text); }
.mbtn-cancel:hover { background: var(--panel-btn-hover); }
.btn-generate {
  padding: 6px 12px; font-size: 0.78rem; border-radius: 4px; cursor: pointer;
  background: var(--panel-btn-bg); color: var(--panel-btn-text);
  border: 1px solid var(--input-border); font-family: inherit; white-space: nowrap;
}
.btn-generate:hover { background: var(--panel-btn-hover); }
.dest-help-btn {
  display: inline-flex; align-items: center; justify-content: center;
  width: 16px; height: 16px; border-radius: 50%; border: 1px solid var(--input-border);
  background: transparent; color: var(--panel-path-text); font-size: 0.65rem;
  cursor: pointer; padding: 0; margin-left: 4px; vertical-align: middle;
  line-height: 1;
}
.dest-help-btn:hover { background: var(--input-border); color: var(--panel-text); }
.dest-help-content table { width: 100%; border-collapse: collapse; margin: 8px 0; font-size: 0.78rem; }
.dest-help-content th { text-align: left; padding: 4px 8px; border-bottom: 1px solid var(--input-border); color: var(--panel-path-text); font-weight: 600; }
.dest-help-content td { padding: 4px 8px; border-bottom: 1px solid var(--input-border); }
.dest-help-content code { background: var(--input-bg); padding: 1px 4px; border-radius: 3px; font-size: 0.76rem; }
.dest-help-content h2 { font-size: 0.9rem; margin: 16px 0 6px; color: var(--panel-heading); }
.dest-help-content h3 { font-size: 0.82rem; margin: 12px 0 4px; color: var(--panel-heading); }
.dest-help-content p { margin: 6px 0; }

.form-row { margin-bottom: 12px; }
.form-row:last-child { margin-bottom: 0; }
.form-row label {
  display: block; font-size: 0.78rem; font-weight: 500;
  color: var(--panel-path-text); margin-bottom: 4px;
}
.form-row input {
  width: 100%; padding: 8px 10px; font-size: 0.85rem;
  background: var(--input-bg); border: 1px solid var(--input-border);
  border-radius: 4px; color: var(--input-text); outline: none;
  font-family: inherit; transition: border-color 0.15s;
}
.form-row input:focus { border-color: var(--btn-bg); }
.form-row input::placeholder { color: var(--input-placeholder); }
.form-row input[readonly] {
  opacity: 0.6; cursor: default;
}
.hostname-prefix { font-weight: 700; }
.shortname-preview { color: var(--panel-heading); text-decoration: none; cursor: default; }
.shortname-preview.has-name { cursor: pointer; }
.shortname-preview.has-name:hover { text-decoration: underline; }
.shortname-live { font-weight: 700; }
.shortname-hints {
  position: absolute; left: 0; right: 0; top: 100%;
  max-height: 120px; overflow-y: auto; z-index: 10;
  background: var(--input-bg); border: 1px solid var(--input-border);
  border-top: none; border-radius: 0 0 4px 4px;
  font-size: 0.82rem;
}
.shortname-hints div {
  padding: 5px 10px; cursor: default; color: var(--input-text);
}
.shortname-hints div.taken {
  color: var(--badge-text, #fff); background: var(--badge-bg, #e74c3c);
}
.color-picker { display: flex; gap: 6px; flex-wrap: wrap; padding: 4px 0; }
.color-swatch {
  width: 20px; height: 20px; border-radius: 4px; cursor: pointer;
  border: 2px solid transparent; transition: border-color 0.15s;
  flex-shrink: 0;
}
.color-swatch:hover { opacity: 0.8; }
.color-swatch.selected { border-color: var(--panel-text); }
.icon-input {
  display: flex; align-items: center; gap: 0;
  background: var(--input-bg); border: 1px solid var(--input-border);
  border-radius: 4px; transition: border-color 0.15s;
}
.icon-input:focus-within { border-color: var(--btn-bg); }
.icon-input .icon-prefix {
  display: flex; align-items: center; justify-content: center;
  width: 32px; height: 32px; flex-shrink: 0; padding: 0 6px;
}
.icon-input .icon-prefix img, .icon-input .icon-prefix svg { width: 16px; height: 16px; }
.icon-input input {
  flex: 1; padding: 8px 10px 8px 0; font-size: 0.85rem;
  background: transparent; border: none; color: var(--input-text);
  outline: none; font-family: inherit;
}
.icon-input input::placeholder { color: var(--input-placeholder); }
.stat-row {
  display: flex; gap: 16px; font-size: 0.78rem; color: var(--panel-path-text);
  padding: 8px 0; border-top: 1px solid var(--panel-border); margin-top: 12px;
}
.stat-item { display: flex; flex-direction: column; gap: 2px; }
.stat-label { font-size: 0.7rem; color: var(--panel-path-text); }
.stat-value { font-size: 0.82rem; color: var(--panel-text); }

/* Toast */
.toast {
  position: fixed; bottom: 20px; right: 20px; padding: 10px 18px;
  border-radius: 6px; font-size: 0.82rem; z-index: 10000;
  display: none; font-weight: 500;
}
.toast.success { background: #a6e3a1; color: #1e1e2e; }
.toast.error { background: #f38ba8; color: #1e1e2e; }
.toast.visible { display: block; }

/* Scrollbar styling */
/* Charts view */
#charts-container {
  flex: 1; overflow: auto; padding: 16px; border-radius: 8px;
  border: 1px solid var(--panel-border); background: var(--panel-bg);
}
.stats-summary { display: flex; gap: 12px; margin-bottom: 20px; flex-wrap: wrap; }
.stat-card {
  flex: 1; min-width: 140px; padding: 16px; border-radius: 8px;
  background: var(--bar-bg); border: 1px solid var(--panel-border); text-align: center;
}
.stat-card .stat-value { font-size: 1.6rem; font-weight: 700; color: var(--panel-heading); }
.stat-card .stat-label { font-size: 0.75rem; color: var(--panel-path-text); margin-top: 4px; }
.charts-row { display: flex; gap: 16px; flex-wrap: wrap; }
.chart-panel {
  flex: 1; min-width: 300px; padding: 16px; border-radius: 8px;
  background: var(--bar-bg); border: 1px solid var(--panel-border);
}
.chart-title { font-size: 0.85rem; font-weight: 600; color: var(--panel-heading); margin-bottom: 12px; }
.top-bar-row { display: flex; align-items: center; gap: 8px; margin-bottom: 6px; cursor: pointer; }
.top-bar-row:hover .top-bar-fill { filter: brightness(1.15); }
.top-bar-name {
  width: 100px; font-size: 0.78rem; color: var(--panel-text); text-align: right;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.top-bar-track { flex: 1; height: 20px; background: var(--input-bg); border-radius: 4px; overflow: hidden; }
.top-bar-fill { height: 100%; background: var(--btn-bg); border-radius: 4px; transition: width 0.3s; }
.top-bar-count { width: 50px; font-size: 0.72rem; color: var(--panel-path-text); }
.daily-chart { display: flex; align-items: flex-end; gap: 2px; height: 160px; }
.daily-bar {
  flex: 1; background: var(--btn-bg); border-radius: 2px 2px 0 0;
  min-width: 4px; transition: height 0.3s; cursor: default;
}
.daily-bar:hover { filter: brightness(1.2); }
.daily-labels { display: flex; justify-content: space-between; font-size: 0.65rem; color: var(--panel-path-text); margin-top: 4px; }
.chart-empty { text-align: center; color: var(--panel-path-text); font-size: 0.82rem; padding: 40px 0; }

#grid-container::-webkit-scrollbar { width: 10px; height: 10px; }
#grid-container::-webkit-scrollbar-track { background: var(--panel-bg); }
#grid-container::-webkit-scrollbar-thumb {
  background: var(--panel-btn-bg); border-radius: 5px;
}
#grid-container::-webkit-scrollbar-thumb:hover { background: var(--panel-btn-hover); }
#grid-container::-webkit-scrollbar-corner { background: var(--panel-bg); }
</style>
</head>
<body>

<div id="headerbar">
  <span class="brand">
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
      <path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"/>
      <path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"/>
    </svg>
    <span>Go<span class="accent">Linx</span></span>
  </span>
  <select id="themeSelect" onchange="applyTheme(this.value)" tabindex="-1">
    <option value="catppuccin-mocha">Catppuccin Mocha</option>
    <option value="dracula">Dracula</option>
    <option value="nord">Nord</option>
    <option value="solarized-dark">Solarized Dark</option>
    <option value="solarized-light">Solarized Light</option>
    <option value="one-dark">One Dark</option>
    <option value="gruvbox">Gruvbox</option>
    <option value="monokai-dimmed">Monokai Dimmed</option>
    <option value="abyss">Abyss</option>
    <option value="catppuccin-latte">Catppuccin Latte</option>
    <option value="github-light">GitHub Light</option>
    <option value="ibm-3278">IBM 3278 Retro</option>
  </select>
  <label id="adminToggle" class="admin-toggle hidden" title="Admin mode — bypass ownership checks">
    <input type="checkbox" id="adminCheck" onchange="toggleAdminMode(this.checked)" />
    <span class="admin-slider"></span>
    <span class="admin-label">Admin</span>
  </label>
</div>

<div id="main">
  <div id="search-area">
    <input type="text" id="searchInput" placeholder="Search..." spellcheck="false" />
    <button id="addBtn" onclick="showNewLinxModal()" title="Add Linx" tabindex="-1">+</button>
  </div>
  <div id="toolbar">
    <div id="sort-btns">
      <button class="sort-btn sort-active" data-sort="az" onclick="setSortMode('az')" title="Sort A-Z" tabindex="-1">A-Z</button>
      <button class="sort-btn" data-sort="popular" onclick="setSortMode('popular')" title="Sort by most clicked" tabindex="-1">Popular</button>
      <button class="sort-btn" data-sort="recent" onclick="setSortMode('recent')" title="Sort by recently clicked" tabindex="-1">Recent</button>
      <button class="sort-btn" data-sort="charts" onclick="setSortMode('charts')" title="Charts" tabindex="-1">Charts</button>
    </div>
    <div id="view-btns">
      <button id="viewGrid" class="view-btn view-active" onclick="setViewMode('grid')" title="Grid view" tabindex="-1">
        <svg width="20" height="20" viewBox="0 0 20 20" fill="currentColor" stroke="none"><rect x="1" y="1" width="8" height="8" rx="1.5"/><rect x="11" y="1" width="8" height="8" rx="1.5"/><rect x="1" y="11" width="8" height="8" rx="1.5"/><rect x="11" y="11" width="8" height="8" rx="1.5"/></svg>
      </button>
      <button id="viewList" class="view-btn" onclick="setViewMode('list')" title="List view" tabindex="-1">
        <svg width="20" height="20" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><line x1="1" y1="4" x2="19" y2="4"/><line x1="1" y1="10" x2="19" y2="10"/><line x1="1" y1="16" x2="19" y2="16"/></svg>
      </button>
    </div>
  </div>
  <div id="grid-container">
    <div id="link-grid"></div>
    <div id="no-results" class="hidden">Add some linx by pressing the Add Linx button (+) above.</div>
  </div>
  <div id="charts-container" class="hidden">
    <div id="stats-summary" class="stats-summary"></div>
    <div class="charts-row">
      <div class="chart-panel">
        <div class="chart-title">Top Links</div>
        <div id="chart-top-links"></div>
      </div>
      <div class="chart-panel">
        <div class="chart-title">Daily Clicks (last 30 days)</div>
        <div id="chart-daily-clicks"></div>
      </div>
    </div>
  </div>
</div>

<div id="statusbar">
  <div id="statusbar-left">
    <button id="gearBtn" onclick="toggleGearMenu(event)" title="Settings"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/></svg></button>
    <span id="link-count">0 links</span>
  </div>
  <span style="font-weight:700;color:var(--bar-text)">Go<span class="accent">Linx</span></span>
</div>
<div id="gear-menu">
  <div class="ctx-item" onclick="window.open('/.help','_blank');hideGearMenu()">Help<span style="float:right;color:var(--panel-path-text);font-size:0.75rem">F1</span></div>
</div>

<!-- Context menu -->
<div id="ctx-menu">
  <div id="ctxView" class="ctx-item" onclick="ctxView()">View</div>
  <div id="ctxEdit" class="ctx-item" onclick="ctxEdit()">Edit</div>
  <div id="ctxSep" class="ctx-separator"></div>
  <div id="ctxDelete" class="ctx-item danger" onclick="ctxDelete()">Delete</div>
</div>

<!-- New Linx Modal -->
<div id="newOverlay" class="modal-overlay hidden">
  <form class="modal-box" autocomplete="off" onsubmit="return false">
    <div class="modal-title">New Linx</div>
    <div class="modal-body">
      <div class="form-row"><label>Type</label>
        <select id="newType" onchange="toggleNewLinxType()" style="width:100%;padding:8px 10px;font-size:0.85rem;background:var(--input-bg);border:1px solid var(--input-border);border-radius:4px;color:var(--input-text);outline:none;font-family:inherit">
          <option value="link">Link</option>
          <option value="employee">Employee</option>
          <option value="customer">Customer</option>
          <option value="vendor">Vendor</option>
          <option value="document">Document</option>
        </select>
      </div>
      <div class="form-row" style="position:relative"><label>Short Name: <a class="shortname-preview" id="newShortNamePreview" target="_blank" rel="noopener"><span class="hostname-prefix"></span><span class="shortname-live"></span></a></label><div style="display:flex;gap:6px;align-items:center"><input type="text" id="newShortName" placeholder="e.g. github" spellcheck="false" autocomplete="off" style="flex:1" oninput="updateShortNamePreview('new')" /><button type="button" class="btn-generate" id="btnGenerate" onclick="generateShortCode()" title="Generate random short code">Generate</button></div><div id="newShortNameHints" class="shortname-hints hidden"></div></div>
      <div class="form-row"><label>Color</label>
        <div class="color-picker" id="newColorPicker">
          <div class="color-swatch selected" data-color="" style="background:transparent;border:2px dashed var(--input-border)" title="None" onclick="pickColor('new','')"></div>
          <div class="color-swatch" data-color="#ef4444" style="background:#ef4444" title="Red" onclick="pickColor('new','#ef4444')"></div>
          <div class="color-swatch" data-color="#f97316" style="background:#f97316" title="Orange" onclick="pickColor('new','#f97316')"></div>
          <div class="color-swatch" data-color="#f59e0b" style="background:#f59e0b" title="Amber" onclick="pickColor('new','#f59e0b')"></div>
          <div class="color-swatch" data-color="#22c55e" style="background:#22c55e" title="Green" onclick="pickColor('new','#22c55e')"></div>
          <div class="color-swatch" data-color="#14b8a6" style="background:#14b8a6" title="Teal" onclick="pickColor('new','#14b8a6')"></div>
          <div class="color-swatch" data-color="#06b6d4" style="background:#06b6d4" title="Cyan" onclick="pickColor('new','#06b6d4')"></div>
          <div class="color-swatch" data-color="#3b82f6" style="background:#3b82f6" title="Blue" onclick="pickColor('new','#3b82f6')"></div>
          <div class="color-swatch" data-color="#6366f1" style="background:#6366f1" title="Indigo" onclick="pickColor('new','#6366f1')"></div>
          <div class="color-swatch" data-color="#a855f7" style="background:#a855f7" title="Purple" onclick="pickColor('new','#a855f7')"></div>
          <div class="color-swatch" data-color="#ec4899" style="background:#ec4899" title="Pink" onclick="pickColor('new','#ec4899')"></div>
          <div class="color-swatch" data-color="#6b7280" style="background:#6b7280" title="Gray" onclick="pickColor('new','#6b7280')"></div>
        </div>
        <input type="hidden" id="newColor" value="" />
      </div>
      <div id="newLinkFields">
        <div class="form-row"><label>Destination URL <button type="button" class="dest-help-btn" onclick="openDestHelp()" title="URL format help">?</button></label><input type="text" id="newDestURL" placeholder="https://... or local-name" spellcheck="false" /></div>
        <div class="form-row"><label>Description</label><input type="text" id="newDescription" placeholder="Optional description" /></div>
        <div class="form-row"><label>Owner</label><input type="text" id="newOwner" placeholder="Optional owner" spellcheck="false" /></div>
      </div>
      <div id="newPersonFields" class="hidden">
        <div style="display:flex;gap:8px">
          <div class="form-row" style="flex:1"><label>First Name</label><input type="text" id="newFirstName" placeholder="John" /></div>
          <div class="form-row" style="flex:1"><label>Last Name</label><input type="text" id="newLastName" placeholder="Smith" /></div>
        </div>
        <div class="form-row"><label>Title</label><input type="text" id="newTitle" placeholder="Software Engineer" /></div>
        <div class="form-row"><label>Email</label><input type="text" id="newEmail" placeholder="john@example.com" spellcheck="false" /></div>
        <div class="form-row"><label>Phone</label><input type="text" id="newPhone" placeholder="810-454-6786" /></div>
        <div class="form-row"><label>Social</label>
          <div class="icon-input" style="margin-bottom:6px"><span class="icon-prefix"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M2 12h20"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg></span><input type="text" id="newWebLink" placeholder="https://example.com" spellcheck="false" /></div>
          <div class="icon-input" style="margin-bottom:6px"><span class="icon-prefix"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="18" height="18" x="3" y="4" rx="2" ry="2"/><line x1="16" x2="16" y1="2" y2="6"/><line x1="8" x2="8" y1="2" y2="6"/><line x1="3" x2="21" y1="10" y2="10"/></svg></span><input type="text" id="newCalLink" placeholder="https://cal.com/..." spellcheck="false" /></div>
          <div class="icon-input" style="margin-bottom:6px"><span class="icon-prefix"><svg viewBox="0 0 24 24" fill="currentColor"><path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z"/></svg></span><input type="text" id="newXLink" placeholder="https://x.com/..." spellcheck="false" /></div>
          <div class="icon-input"><span class="icon-prefix"><svg viewBox="0 0 24 24" fill="currentColor"><path d="M20.447 20.452h-3.554v-5.569c0-1.328-.027-3.037-1.852-3.037-1.853 0-2.136 1.445-2.136 2.939v5.667H9.351V9h3.414v1.561h.046c.477-.9 1.637-1.85 3.37-1.85 3.601 0 4.267 2.37 4.267 5.455v6.286zM5.337 7.433a2.062 2.062 0 0 1-2.063-2.065 2.064 2.064 0 1 1 2.063 2.065zm1.782 13.019H3.555V9h3.564v11.452zM22.225 0H1.771C.792 0 0 .774 0 1.729v20.542C0 23.227.792 24 1.771 24h20.451C23.2 24 24 23.227 24 22.271V1.729C24 .774 23.2 0 22.222 0h.003z"/></svg></span><input type="text" id="newLinkedInLink" placeholder="https://linkedin.com/in/..." spellcheck="false" /></div>
        </div>
      </div>
      <div id="newDocumentFields" class="hidden">
        <div class="form-row"><label>Title</label><input type="text" id="newDocTitle" placeholder="Document title" /></div>
        <div class="form-row"><label>Format</label>
          <select id="newDocFormat" style="width:100%;padding:8px 10px;font-size:0.85rem;background:var(--input-bg);border:1px solid var(--input-border);border-radius:4px;color:var(--input-text);outline:none;font-family:inherit">
            <option value="text/markdown">Markdown</option>
            <option value="text/html">HTML</option>
            <option value="text/plain">Plain Text</option>
          </select>
        </div>
        <div class="form-row"><label>Content</label>
          <input type="file" id="newDocFile" accept=".md,.markdown,.html,.htm,.txt,.text" style="font-size:0.78rem;color:var(--panel-text);margin-bottom:6px" />
          <textarea id="newDocContent" placeholder="Enter or paste content..." rows="10" style="width:100%;font-family:'SFMono-Regular',Consolas,monospace;font-size:0.82rem;padding:8px 10px;background:var(--input-bg);border:1px solid var(--input-border);border-radius:4px;color:var(--input-text);resize:vertical;outline:none"></textarea>
        </div>
        <div class="form-row"><label>Owner</label><input type="text" id="newDocOwner" placeholder="Optional owner" spellcheck="false" /></div>
      </div>
      <div class="form-row"><label>Tags</label><input type="text" id="newTags" placeholder="tag1, tag2, ..." spellcheck="false" autocomplete="off" /></div>
    </div>
    <div class="modal-actions">
      <button class="mbtn-cancel" onclick="closeNewLinxModal()">Cancel</button>
      <button class="mbtn-primary" onclick="saveNewLinx()">Save</button>
    </div>
  </form>
</div>

<!-- Edit Linx Modal (unified) -->
<div id="editOverlay" class="modal-overlay hidden">
  <form class="modal-box" autocomplete="off" onsubmit="return false">
    <div class="modal-title" id="editModalTitle">Edit</div>
    <div class="modal-body">
      <div class="form-row" style="position:relative"><label>Short Name: <a class="shortname-preview" id="editShortNamePreview" target="_blank" rel="noopener"><span class="hostname-prefix"></span><span class="shortname-live"></span></a></label><input type="text" id="editShortName" spellcheck="false" autocomplete="off" oninput="updateShortNamePreview('edit')" /><div id="editShortNameHints" class="shortname-hints hidden"></div></div>
      <div class="form-row"><label>Color</label>
        <div class="color-picker" id="editColorPicker">
          <div class="color-swatch selected" data-color="" style="background:transparent;border:2px dashed var(--input-border)" title="None" onclick="pickColor('edit','')"></div>
          <div class="color-swatch" data-color="#ef4444" style="background:#ef4444" title="Red" onclick="pickColor('edit','#ef4444')"></div>
          <div class="color-swatch" data-color="#f97316" style="background:#f97316" title="Orange" onclick="pickColor('edit','#f97316')"></div>
          <div class="color-swatch" data-color="#f59e0b" style="background:#f59e0b" title="Amber" onclick="pickColor('edit','#f59e0b')"></div>
          <div class="color-swatch" data-color="#22c55e" style="background:#22c55e" title="Green" onclick="pickColor('edit','#22c55e')"></div>
          <div class="color-swatch" data-color="#14b8a6" style="background:#14b8a6" title="Teal" onclick="pickColor('edit','#14b8a6')"></div>
          <div class="color-swatch" data-color="#06b6d4" style="background:#06b6d4" title="Cyan" onclick="pickColor('edit','#06b6d4')"></div>
          <div class="color-swatch" data-color="#3b82f6" style="background:#3b82f6" title="Blue" onclick="pickColor('edit','#3b82f6')"></div>
          <div class="color-swatch" data-color="#6366f1" style="background:#6366f1" title="Indigo" onclick="pickColor('edit','#6366f1')"></div>
          <div class="color-swatch" data-color="#a855f7" style="background:#a855f7" title="Purple" onclick="pickColor('edit','#a855f7')"></div>
          <div class="color-swatch" data-color="#ec4899" style="background:#ec4899" title="Pink" onclick="pickColor('edit','#ec4899')"></div>
          <div class="color-swatch" data-color="#6b7280" style="background:#6b7280" title="Gray" onclick="pickColor('edit','#6b7280')"></div>
        </div>
        <input type="hidden" id="editColor" value="" />
      </div>
      <div id="editLinkFields">
        <div class="form-row"><label>Destination URL <button type="button" class="dest-help-btn" onclick="openDestHelp()" title="URL format help">?</button></label><input type="text" id="editDestURL" spellcheck="false" /></div>
        <div class="form-row"><label>Description</label><input type="text" id="editDescription" /></div>
        <div class="form-row"><label>Owner</label><input type="text" id="editOwner" spellcheck="false" /></div>
      </div>
      <div id="editPersonFields" class="hidden">
        <div class="form-row">
          <label>Avatar</label>
          <div style="display:flex;align-items:center;gap:12px">
            <div id="editAvatarPreview" class="profile-avatar"></div>
            <input type="file" id="editAvatarFile" accept="image/*" style="font-size:0.78rem;color:var(--panel-text)" />
          </div>
        </div>
        <div style="display:flex;gap:8px">
          <div class="form-row" style="flex:1"><label>First Name</label><input type="text" id="editFirstName" /></div>
          <div class="form-row" style="flex:1"><label>Last Name</label><input type="text" id="editLastName" /></div>
        </div>
        <div class="form-row"><label>Title</label><input type="text" id="editTitle" /></div>
        <div class="form-row"><label>Email</label><input type="text" id="editEmail" spellcheck="false" /></div>
        <div class="form-row"><label>Phone</label><input type="text" id="editPhone" /></div>
        <div class="form-row"><label>Social</label>
          <div class="icon-input" style="margin-bottom:6px"><span class="icon-prefix"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><path d="M2 12h20"/><path d="M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg></span><input type="text" id="editWebLink" placeholder="https://example.com" spellcheck="false" /></div>
          <div class="icon-input" style="margin-bottom:6px"><span class="icon-prefix"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="18" height="18" x="3" y="4" rx="2" ry="2"/><line x1="16" x2="16" y1="2" y2="6"/><line x1="8" x2="8" y1="2" y2="6"/><line x1="3" x2="21" y1="10" y2="10"/></svg></span><input type="text" id="editCalLink" spellcheck="false" /></div>
          <div class="icon-input" style="margin-bottom:6px"><span class="icon-prefix"><svg viewBox="0 0 24 24" fill="currentColor"><path d="M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z"/></svg></span><input type="text" id="editXLink" spellcheck="false" /></div>
          <div class="icon-input"><span class="icon-prefix"><svg viewBox="0 0 24 24" fill="currentColor"><path d="M20.447 20.452h-3.554v-5.569c0-1.328-.027-3.037-1.852-3.037-1.853 0-2.136 1.445-2.136 2.939v5.667H9.351V9h3.414v1.561h.046c.477-.9 1.637-1.85 3.37-1.85 3.601 0 4.267 2.37 4.267 5.455v6.286zM5.337 7.433a2.062 2.062 0 0 1-2.063-2.065 2.064 2.064 0 1 1 2.063 2.065zm1.782 13.019H3.555V9h3.564v11.452zM22.225 0H1.771C.792 0 0 .774 0 1.729v20.542C0 23.227.792 24 1.771 24h20.451C23.2 24 24 23.227 24 22.271V1.729C24 .774 23.2 0 22.222 0h.003z"/></svg></span><input type="text" id="editLinkedInLink" spellcheck="false" /></div>
        </div>
      </div>
      <div id="editDocumentFields" class="hidden">
        <div class="form-row"><label>Title</label><input type="text" id="editDocTitle" /></div>
        <div class="form-row"><label>Format</label>
          <select id="editDocFormat" style="width:100%;padding:8px 10px;font-size:0.85rem;background:var(--input-bg);border:1px solid var(--input-border);border-radius:4px;color:var(--input-text);outline:none;font-family:inherit">
            <option value="text/markdown">Markdown</option>
            <option value="text/html">HTML</option>
            <option value="text/plain">Plain Text</option>
          </select>
        </div>
        <div class="form-row"><label>Content</label>
          <input type="file" id="editDocFile" accept=".md,.markdown,.html,.htm,.txt,.text" style="font-size:0.78rem;color:var(--panel-text);margin-bottom:6px" />
          <textarea id="editDocContent" rows="10" style="width:100%;font-family:'SFMono-Regular',Consolas,monospace;font-size:0.82rem;padding:8px 10px;background:var(--input-bg);border:1px solid var(--input-border);border-radius:4px;color:var(--input-text);resize:vertical;outline:none"></textarea>
        </div>
        <div class="form-row"><label>Owner</label><input type="text" id="editDocOwner" spellcheck="false" /></div>
      </div>
      <div class="form-row"><label>Tags</label><input type="text" id="editTags" placeholder="tag1, tag2, ..." spellcheck="false" autocomplete="off" /></div>
      <div class="stat-row">
        <div class="stat-item"><span class="stat-label">Created</span><span class="stat-value" id="editCreated">-</span></div>
        <div class="stat-item"><span class="stat-label">Last Clicked</span><span class="stat-value" id="editLastClicked">-</span></div>
        <div class="stat-item"><span class="stat-label">Clicks</span><span class="stat-value" id="editClicks">0</span></div>
      </div>
    </div>
    <div class="modal-actions">
      <button id="editCancelBtn" class="mbtn-cancel" onclick="closeEditModal()">Cancel</button>
      <button id="editSaveBtn" class="mbtn-primary" onclick="saveEditLinx()">Save</button>
    </div>
  </form>
</div>

<!-- Delete Confirmation Modal -->
<div id="deleteOverlay" class="modal-overlay hidden">
  <div class="modal-box">
    <div class="modal-title" id="deleteModalTitle">Delete</div>
    <div class="modal-body">
      <p>Are you sure you want to delete <strong id="deleteShortName"></strong>?</p>
      <p style="font-size:0.78rem;color:var(--panel-path-text);margin-top:6px" id="deleteSubtitle"></p>
    </div>
    <div class="modal-actions">
      <button class="mbtn-cancel" onclick="closeDeleteModal()">Cancel</button>
      <button class="mbtn-danger" onclick="confirmDelete()">Delete</button>
    </div>
  </div>
</div>

<!-- Destination URL Help Modal -->
<div id="destHelpOverlay" class="modal-overlay hidden">
  <div class="modal-box" style="max-width:750px;max-height:80vh;display:flex;flex-direction:column">
    <div class="modal-title">Destination URL Help</div>
    <div class="modal-body dest-help-content" style="overflow-y:auto;padding-right:24px">
      <!--DEST_URL_HELP-->
    </div>
    <div class="modal-actions">
      <button class="mbtn-cancel" onclick="closeDestHelp()">Close</button>
    </div>
  </div>
</div>

<!-- Toast -->
<div id="toast" class="toast"></div>

<script>
var themes = {
  'catppuccin-mocha': {
    '--bar-bg':'#1e1e2e','--bar-border':'#313244','--bar-text':'#cdd6f4',
    '--input-bg':'#313244','--input-border':'#45475a','--input-text':'#cdd6f4',
    '--input-placeholder':'#6c7086',
    '--btn-bg':'#89b4fa','--btn-text':'#1e1e2e','--btn-hover':'#74c7ec',
    '--btn-danger-bg':'#f38ba8','--btn-danger-hover':'#eba0ac',
    '--panel-bg':'#1e1e2e','--panel-border':'#313244','--panel-text':'#cdd6f4',
    '--panel-heading':'#89b4fa','--panel-path-text':'#a6adc8',
    '--panel-path-border':'#313244','--panel-hover':'#313244',
    '--panel-btn-bg':'#45475a','--panel-btn-text':'#cdd6f4',
    '--panel-btn-hover':'#585b70','--check-accent':'#89b4fa',
    '--body-bg':'#11111b'
  },
  'dracula': {
    '--bar-bg':'#282a36','--bar-border':'#44475a','--bar-text':'#f8f8f2',
    '--input-bg':'#44475a','--input-border':'#6272a4','--input-text':'#f8f8f2',
    '--input-placeholder':'#6272a4',
    '--btn-bg':'#bd93f9','--btn-text':'#282a36','--btn-hover':'#caa9fa',
    '--btn-danger-bg':'#ff5555','--btn-danger-hover':'#ff6e6e',
    '--panel-bg':'#282a36','--panel-border':'#44475a','--panel-text':'#f8f8f2',
    '--panel-heading':'#bd93f9','--panel-path-text':'#6272a4',
    '--panel-path-border':'#44475a','--panel-hover':'#44475a',
    '--panel-btn-bg':'#44475a','--panel-btn-text':'#f8f8f2',
    '--panel-btn-hover':'#6272a4','--check-accent':'#bd93f9',
    '--body-bg':'#1e1f29'
  },
  'nord': {
    '--bar-bg':'#2e3440','--bar-border':'#3b4252','--bar-text':'#d8dee9',
    '--input-bg':'#3b4252','--input-border':'#434c5e','--input-text':'#eceff4',
    '--input-placeholder':'#4c566a',
    '--btn-bg':'#88c0d0','--btn-text':'#2e3440','--btn-hover':'#8fbcbb',
    '--btn-danger-bg':'#bf616a','--btn-danger-hover':'#d08770',
    '--panel-bg':'#2e3440','--panel-border':'#3b4252','--panel-text':'#d8dee9',
    '--panel-heading':'#88c0d0','--panel-path-text':'#4c566a',
    '--panel-path-border':'#3b4252','--panel-hover':'#3b4252',
    '--panel-btn-bg':'#434c5e','--panel-btn-text':'#d8dee9',
    '--panel-btn-hover':'#4c566a','--check-accent':'#88c0d0',
    '--body-bg':'#242933'
  },
  'solarized-dark': {
    '--bar-bg':'#002b36','--bar-border':'#073642','--bar-text':'#839496',
    '--input-bg':'#073642','--input-border':'#586e75','--input-text':'#93a1a1',
    '--input-placeholder':'#586e75',
    '--btn-bg':'#268bd2','--btn-text':'#fdf6e3','--btn-hover':'#2aa198',
    '--btn-danger-bg':'#dc322f','--btn-danger-hover':'#cb4b16',
    '--panel-bg':'#002b36','--panel-border':'#073642','--panel-text':'#839496',
    '--panel-heading':'#268bd2','--panel-path-text':'#586e75',
    '--panel-path-border':'#073642','--panel-hover':'#073642',
    '--panel-btn-bg':'#073642','--panel-btn-text':'#93a1a1',
    '--panel-btn-hover':'#586e75','--check-accent':'#268bd2',
    '--body-bg':'#001e26'
  },
  'solarized-light': {
    '--bar-bg':'#eee8d5','--bar-border':'#ddd6c1','--bar-text':'#657b83',
    '--input-bg':'#fdf6e3','--input-border':'#d3cbb7','--input-text':'#586e75',
    '--input-placeholder':'#93a1a1',
    '--btn-bg':'#268bd2','--btn-text':'#fdf6e3','--btn-hover':'#2aa198',
    '--btn-danger-bg':'#dc322f','--btn-danger-hover':'#cb4b16',
    '--panel-bg':'#eee8d5','--panel-border':'#ddd6c1','--panel-text':'#657b83',
    '--panel-heading':'#268bd2','--panel-path-text':'#93a1a1',
    '--panel-path-border':'#ddd6c1','--panel-hover':'#fdf6e3',
    '--panel-btn-bg':'#fdf6e3','--panel-btn-text':'#586e75',
    '--panel-btn-hover':'#ddd6c1','--check-accent':'#268bd2',
    '--body-bg':'#fdf6e3'
  },
  'one-dark': {
    '--bar-bg':'#21252b','--bar-border':'#181a1f','--bar-text':'#abb2bf',
    '--input-bg':'#2c313a','--input-border':'#3e4451','--input-text':'#abb2bf',
    '--input-placeholder':'#5c6370',
    '--btn-bg':'#61afef','--btn-text':'#21252b','--btn-hover':'#528bff',
    '--btn-danger-bg':'#e06c75','--btn-danger-hover':'#be5046',
    '--panel-bg':'#21252b','--panel-border':'#181a1f','--panel-text':'#abb2bf',
    '--panel-heading':'#61afef','--panel-path-text':'#5c6370',
    '--panel-path-border':'#181a1f','--panel-hover':'#2c313a',
    '--panel-btn-bg':'#3e4451','--panel-btn-text':'#abb2bf',
    '--panel-btn-hover':'#4b5263','--check-accent':'#61afef',
    '--body-bg':'#1b1f23'
  },
  'gruvbox': {
    '--bar-bg':'#282828','--bar-border':'#3c3836','--bar-text':'#ebdbb2',
    '--input-bg':'#3c3836','--input-border':'#504945','--input-text':'#ebdbb2',
    '--input-placeholder':'#665c54',
    '--btn-bg':'#b8bb26','--btn-text':'#282828','--btn-hover':'#98971a',
    '--btn-danger-bg':'#fb4934','--btn-danger-hover':'#cc241d',
    '--panel-bg':'#282828','--panel-border':'#3c3836','--panel-text':'#ebdbb2',
    '--panel-heading':'#b8bb26','--panel-path-text':'#a89984',
    '--panel-path-border':'#3c3836','--panel-hover':'#3c3836',
    '--panel-btn-bg':'#504945','--panel-btn-text':'#ebdbb2',
    '--panel-btn-hover':'#665c54','--check-accent':'#b8bb26',
    '--body-bg':'#1d2021'
  },
  'monokai-dimmed': {
    '--bar-bg':'#353535','--bar-border':'#505050','--bar-text':'#d8d8d8',
    '--input-bg':'#525252','--input-border':'#505050','--input-text':'#c5c8c6',
    '--input-placeholder':'#949494',
    '--btn-bg':'#565656','--btn-text':'#ffffff','--btn-hover':'#707070',
    '--btn-danger-bg':'#c4625b','--btn-danger-hover':'#d4736c',
    '--panel-bg':'#1e1e1e','--panel-border':'#303030','--panel-text':'#c5c8c6',
    '--panel-heading':'#e58520','--panel-path-text':'#949494',
    '--panel-path-border':'#303030','--panel-hover':'#444444',
    '--panel-btn-bg':'#505050','--panel-btn-text':'#d8d8d8',
    '--panel-btn-hover':'#565656','--check-accent':'#3655b5',
    '--body-bg':'#1e1e1e'
  },
  'abyss': {
    '--bar-bg':'#000c18','--bar-border':'#082050','--bar-text':'#6688cc',
    '--input-bg':'#082050','--input-border':'#0a3074','--input-text':'#6688cc',
    '--input-placeholder':'#384887',
    '--btn-bg':'#225588','--btn-text':'#ddbb88','--btn-hover':'#2277aa',
    '--btn-danger-bg':'#994444','--btn-danger-hover':'#bb5555',
    '--panel-bg':'#000c18','--panel-border':'#082050','--panel-text':'#6688cc',
    '--panel-heading':'#225588','--panel-path-text':'#384887',
    '--panel-path-border':'#082050','--panel-hover':'#082050',
    '--panel-btn-bg':'#0a3074','--panel-btn-text':'#6688cc',
    '--panel-btn-hover':'#103880','--check-accent':'#225588',
    '--body-bg':'#000c18'
  },
  'catppuccin-latte': {
    '--bar-bg':'#e6e9ef','--bar-border':'#ccd0da','--bar-text':'#4c4f69',
    '--input-bg':'#eff1f5','--input-border':'#bcc0cc','--input-text':'#4c4f69',
    '--input-placeholder':'#9ca0b0',
    '--btn-bg':'#1e66f5','--btn-text':'#eff1f5','--btn-hover':'#209fb5',
    '--btn-danger-bg':'#d20f39','--btn-danger-hover':'#e64553',
    '--panel-bg':'#eff1f5','--panel-border':'#ccd0da','--panel-text':'#4c4f69',
    '--panel-heading':'#1e66f5','--panel-path-text':'#6c6f85',
    '--panel-path-border':'#ccd0da','--panel-hover':'#e6e9ef',
    '--panel-btn-bg':'#ccd0da','--panel-btn-text':'#4c4f69',
    '--panel-btn-hover':'#bcc0cc','--check-accent':'#1e66f5',
    '--body-bg':'#dce0e8'
  },
  'github-light': {
    '--bar-bg':'#f6f8fa','--bar-border':'#d0d7de','--bar-text':'#1f2328',
    '--input-bg':'#ffffff','--input-border':'#d0d7de','--input-text':'#1f2328',
    '--input-placeholder':'#6e7781',
    '--btn-bg':'#0969da','--btn-text':'#ffffff','--btn-hover':'#0550ae',
    '--btn-danger-bg':'#cf222e','--btn-danger-hover':'#a40e26',
    '--panel-bg':'#ffffff','--panel-border':'#d0d7de','--panel-text':'#1f2328',
    '--panel-heading':'#0969da','--panel-path-text':'#656d76',
    '--panel-path-border':'#d0d7de','--panel-hover':'#f6f8fa',
    '--panel-btn-bg':'#f6f8fa','--panel-btn-text':'#1f2328',
    '--panel-btn-hover':'#eaeef2','--check-accent':'#0969da',
    '--body-bg':'#f0f3f6'
  },
  'ibm-3278': {
    '--bar-bg':'#020602','--bar-border':'#0a3a0a','--bar-text':'#33ff33',
    '--input-bg':'#050a05','--input-border':'#0a3a0a','--input-text':'#33ff33',
    '--input-placeholder':'#1a6b1a',
    '--btn-bg':'#33ff33','--btn-text':'#020602','--btn-hover':'#66ff66',
    '--btn-danger-bg':'#ff4444','--btn-danger-hover':'#ff6666',
    '--panel-bg':'#020602','--panel-border':'#0a3a0a','--panel-text':'#33ff33',
    '--panel-heading':'#66ff66','--panel-path-text':'#1a9a1a',
    '--panel-path-border':'#0a3a0a','--panel-hover':'#0a1a0a',
    '--panel-btn-bg':'#0a3a0a','--panel-btn-text':'#33ff33',
    '--panel-btn-hover':'#0d4d0d','--check-accent':'#33ff33',
    '--body-bg':'#010401',
    '--font':"'Share Tech Mono', monospace"
  }
};

var allLinx = [];
var filteredLinx = [];
var ctxTarget = null;
var editingLinxId = null;
var editingLinxType = null;
var deletingLinxId = null;
var viewMode = 'grid';
var sortMode = 'az';

function escHtml(s) {
  if (!s) return '';
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function formatTime(unix) {
  if (!unix || unix === 0) return '-';
  var d = new Date(unix * 1000);
  return d.toLocaleString();
}

function fuzzyMatch(needle, haystack) {
  needle = needle.toLowerCase();
  haystack = haystack.toLowerCase();
  if (needle.length < 2) return false;
  var ni = 0;
  var firstMatch = -1;
  var lastMatch = -1;
  for (var hi = 0; hi < haystack.length && ni < needle.length; hi++) {
    if (haystack[hi] === needle[ni]) {
      if (firstMatch === -1) firstMatch = hi;
      lastMatch = hi;
      ni++;
    }
  }
  if (ni !== needle.length) return false;
  var span = lastMatch - firstMatch + 1;
  return span <= needle.length * 3;
}

function itemSortKey(item) {
  if (item.type !== 'link') return (item.firstName + ' ' + item.lastName).toLowerCase();
  return item.shortName.toLowerCase();
}

function sortLinx(arr) {
  var copy = arr.slice();
  if (sortMode === 'popular') {
    copy.sort(function(a, b) { return (b.clickCount || 0) - (a.clickCount || 0); });
  } else if (sortMode === 'recent') {
    copy.sort(function(a, b) {
      var aT = a.type !== 'link' ? (a.dateCreated || 0) : (a.lastClicked || 0);
      var bT = b.type !== 'link' ? (b.dateCreated || 0) : (b.lastClicked || 0);
      return bT - aT;
    });
  } else {
    copy.sort(function(a, b) { return itemSortKey(a).localeCompare(itemSortKey(b)); });
  }
  return copy;
}

function itemSearchText(item) {
  if (item.type === 'document') {
    return [item.shortName, item.description, item.owner, item.tags].join(' ').toLowerCase();
  }
  if (item.type !== 'link') {
    return [item.shortName, item.firstName, item.lastName, item.email, item.tags].join(' ').toLowerCase();
  }
  return [item.shortName, item.destinationURL, item.description, item.owner, item.tags].join(' ').toLowerCase();
}

var typeAliases = {e:'employee',c:'customer',v:'vendor',l:'link',d:'document'};

function filterLinx() {
  var q = document.getElementById('searchInput').value.trim();
  if (!q) {
    filteredLinx = sortLinx(allLinx);
    renderGrid();
    return;
  }

  var typeFilter = '';
  var ql = q;
  var m = q.match(/^:(\w+)\s*(.*)/);
  if (m) {
    var alias = m[1].toLowerCase();
    if (alias === 't') {
      var tagTerms = m[2].split(',');
      var results = [];
      for (var i = 0; i < allLinx.length; i++) {
        var itemTags = (allLinx[i].tags || '').toLowerCase().split(',');
        for (var j = 0; j < itemTags.length; j++) itemTags[j] = itemTags[j].trim();
        for (var ti = 0; ti < tagTerms.length; ti++) {
          var term = tagTerms[ti].trim().toLowerCase();
          if (term && itemTags.indexOf(term) >= 0) { results.push(allLinx[i]); break; }
        }
      }
      filteredLinx = sortLinx(results);
      renderGrid();
      return;
    }
    if (typeAliases[alias]) {
      typeFilter = typeAliases[alias];
      ql = m[2].trim();
    }
  }

  var pool = allLinx;
  if (typeFilter) {
    pool = [];
    for (var i = 0; i < allLinx.length; i++) {
      if (allLinx[i].type === typeFilter) pool.push(allLinx[i]);
    }
  }

  if (!ql) {
    filteredLinx = sortLinx(pool);
    renderGrid();
    return;
  }

  ql = ql.toLowerCase();
  var exact = [];
  var fuzzy = [];
  for (var i = 0; i < pool.length; i++) {
    var item = pool[i];
    var text = itemSearchText(item);
    if (text.indexOf(ql) !== -1) {
      exact.push(item);
    } else if (fuzzyMatch(ql, text)) {
      fuzzy.push(item);
    }
  }
  filteredLinx = sortLinx(exact.length > 0 ? exact : fuzzy);
  renderGrid();
}

function setSortMode(mode) {
  sortMode = mode;
  var btns = document.querySelectorAll('.sort-btn');
  for (var i = 0; i < btns.length; i++) {
    btns[i].className = 'sort-btn' + (btns[i].getAttribute('data-sort') === mode ? ' sort-active' : '');
  }
  if (mode === 'charts') {
    document.getElementById('grid-container').classList.add('hidden');
    document.getElementById('charts-container').classList.remove('hidden');
    loadStats();
  } else {
    document.getElementById('charts-container').classList.add('hidden');
    document.getElementById('grid-container').classList.remove('hidden');
    filterLinx();
  }
  if (!_restoring) fetch('/api/settings', {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({key: 'sortMode', value: mode})
  }).catch(function(){});
}

// Charts / Stats
function loadStats() {
  fetch('/api/stats').then(function(r) { return r.json(); }).then(function(data) {
    renderSummaryCards(data.summary);
    renderTopLinksChart(data.topLinks);
    renderDailyClicksChart(data.dailyClicks);
  }).catch(function() {});
}

function renderSummaryCards(s) {
  var el = document.getElementById('stats-summary');
  el.innerHTML =
    '<div class="stat-card"><div class="stat-value">' + (s.totalLinks || 0) + '</div><div class="stat-label">Total Links</div></div>' +
    '<div class="stat-card"><div class="stat-value">' + (s.totalClicks || 0) + '</div><div class="stat-label">Total Clicks</div></div>' +
    '<div class="stat-card"><div class="stat-value">' + escHtml(s.topLink || '—') + '</div><div class="stat-label">Top Link</div></div>' +
    '<div class="stat-card"><div class="stat-value">' + (s.createdThisWeek || 0) + '</div><div class="stat-label">Created This Week</div></div>';
}

function renderTopLinksChart(links) {
  var el = document.getElementById('chart-top-links');
  if (!links || links.length === 0) {
    el.innerHTML = '<div class="chart-empty">No click data yet</div>';
    return;
  }
  var max = links[0].clickCount || 1;
  var html = '';
  for (var i = 0; i < links.length; i++) {
    var pct = Math.round((links[i].clickCount / max) * 100);
    html += '<div class="top-bar-row" onclick="window.location.href=\'/\' + decodeURIComponent(\'' + encodeURIComponent(links[i].shortName) + '\') + \'+\'">' +
      '<div class="top-bar-name" title="' + escHtml(links[i].shortName) + '">' + escHtml(links[i].shortName) + '</div>' +
      '<div class="top-bar-track"><div class="top-bar-fill" style="width:' + pct + '%"></div></div>' +
      '<div class="top-bar-count">' + links[i].clickCount + '</div>' +
    '</div>';
  }
  el.innerHTML = html;
}

function renderDailyClicksChart(days) {
  var el = document.getElementById('chart-daily-clicks');
  if (!days || days.length === 0) {
    el.innerHTML = '<div class="chart-empty">No click data yet — clicks will appear here as links are used</div>';
    return;
  }
  // Fill in missing days to get a continuous 30-day range
  var dayMap = {};
  for (var i = 0; i < days.length; i++) dayMap[days[i].date] = days[i].count;
  var filled = [];
  var now = new Date();
  for (var d = 29; d >= 0; d--) {
    var dt = new Date(now);
    dt.setDate(dt.getDate() - d);
    var key = dt.toISOString().slice(0, 10);
    filled.push({date: key, count: dayMap[key] || 0});
  }
  var max = 1;
  for (var i = 0; i < filled.length; i++) { if (filled[i].count > max) max = filled[i].count; }
  var barsHtml = '';
  for (var i = 0; i < filled.length; i++) {
    var h = filled[i].count > 0 ? Math.max(4, Math.round((filled[i].count / max) * 156)) : 0;
    barsHtml += '<div class="daily-bar" style="height:' + h + 'px" title="' + filled[i].date + ': ' + filled[i].count + ' clicks"></div>';
  }
  var labels = '<span>' + filled[0].date.slice(5) + '</span>';
  var mid = Math.floor(filled.length / 2);
  labels += '<span>' + filled[mid].date.slice(5) + '</span>';
  labels += '<span>' + filled[filled.length - 1].date.slice(5) + '</span>';
  el.innerHTML = '<div class="daily-chart">' + barsHtml + '</div><div class="daily-labels">' + labels + '</div>';
}

var badgeLabels = {employee: 'Emp', customer: 'Cus', vendor: 'Ven', document: 'Doc'};
function typeBadge(t) {
  return badgeLabels[t] || t.charAt(0).toUpperCase() + t.slice(1);
}

function renderPersonLinx(c) {
  var avatarHtml = c.avatarMime
    ? '<img src="/api/linx/' + c.id + '/avatar" alt="' + escHtml(c.firstName) + '" />'
    : '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="8" r="4"/><path d="M20 21a8 8 0 0 0-16 0"/></svg>';
  var pcStyle = c.color ? ' style="border-left-color:' + c.color + '"' : '';
  return '<div class="linx-item profile-linx" data-id="' + c.id + '"' + pcStyle + ' tabindex="0" oncontextmenu="showCtxMenu(event,' + c.id + ')" ondblclick="dblClickLinx(' + c.id + ')">'
    + '<div class="profile-linx-body">'
    + '<div class="profile-avatar">' + avatarHtml + '</div>'
    + '<div class="profile-info">'
    + '<span class="linx-shortname">' + escHtml(c.firstName + ' ' + c.lastName) + '</span>'
    + (c.title ? '<div class="profile-email">' + escHtml(c.title) + '</div>' : '')
    + (c.email ? '<div class="profile-email">' + escHtml(c.email) + '</div>' : '')
    + '<div class="profile-short">/' + escHtml(c.shortName) + '</div>'
    + '</div></div>'
    + renderTags(c.tags)
    + '<div class="linx-meta"><span></span><span class="linx-badge">' + escHtml(typeBadge(c.type)) + '</span></div>'
    + '</div>';
}

function renderDocumentLinx(c) {
  var docIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><polyline points="14 2 14 8 20 8"/><line x1="16" y1="13" x2="8" y2="13"/><line x1="16" y1="17" x2="8" y2="17"/><polyline points="10 9 9 9 8 9"/></svg>';
  var dcStyle = c.color ? ' style="border-left-color:' + c.color + '"' : '';
  return '<div class="linx-item doc-linx" data-id="' + c.id + '"' + dcStyle + ' tabindex="0" oncontextmenu="showCtxMenu(event,' + c.id + ')" ondblclick="dblClickLinx(' + c.id + ')">'
    + '<div class="doc-linx-body">'
    + '<div class="doc-icon">' + docIcon + '</div>'
    + '<div class="doc-info">'
    + '<span class="linx-shortname">' + escHtml(c.description || c.shortName) + '</span>'
    + '<div class="doc-short">/' + escHtml(c.shortName) + '</div>'
    + '</div></div>'
    + renderTags(c.tags)
    + '<div class="linx-meta">'
    + (c.owner ? '<span class="linx-owner">' + escHtml(c.owner) + '</span>' : '<span></span>')
    + '<span class="linx-badge">' + escHtml(typeBadge(c.type)) + '</span>'
    + '</div></div>';
}

function renderTags(tags) {
  if (!tags) return '';
  var parts = tags.split(',');
  var h = '<div class="linx-tags">';
  for (var i = 0; i < parts.length; i++) {
    var t = parts[i].trim();
    if (t) h += '<span class="tag-badge">' + escHtml(t) + '</span>';
  }
  return h + '</div>';
}

// Tag autocomplete
function getAllTags() {
  var tagSet = {};
  for (var i = 0; i < allLinx.length; i++) {
    var tags = allLinx[i].tags;
    if (!tags) continue;
    var parts = tags.split(',');
    for (var j = 0; j < parts.length; j++) {
      var t = parts[j].trim().toLowerCase();
      if (t) tagSet[t] = true;
    }
  }
  return Object.keys(tagSet).sort();
}

function setupTagAutocomplete(inputId) {
  var input = document.getElementById(inputId);
  if (!input) return;
  var wrap = input.parentNode;
  wrap.style.position = 'relative';
  var dd = document.createElement('div');
  dd.className = 'tag-autocomplete hidden';
  wrap.appendChild(dd);

  function currentTag() {
    var val = input.value;
    var ci = input.selectionStart || val.length;
    var start = val.lastIndexOf(',', ci - 1) + 1;
    return { text: val.substring(start).trimStart().toLowerCase(), start: start, ci: ci };
  }

  function update() {
    var ct = currentTag();
    if (!ct.text) { dd.classList.add('hidden'); return; }
    var existing = input.value.toLowerCase().split(',');
    for (var i = 0; i < existing.length; i++) existing[i] = existing[i].trim();
    var all = getAllTags();
    var matches = [];
    for (var i = 0; i < all.length; i++) {
      if (all[i].indexOf(ct.text) !== -1 && existing.indexOf(all[i]) === -1) {
        matches.push(all[i]);
      }
    }
    if (matches.length === 0) { dd.classList.add('hidden'); return; }
    var h = '';
    for (var i = 0; i < matches.length && i < 8; i++) {
      h += '<div class="tag-ac-item" data-tag="' + escHtml(matches[i]) + '">' + escHtml(matches[i]) + '</div>';
    }
    dd.innerHTML = h;
    dd.classList.remove('hidden');
  }

  function pickTag(tag) {
    var ct = currentTag();
    var before = input.value.substring(0, ct.start);
    var afterComma = input.value.indexOf(',', ct.ci);
    var after = afterComma >= 0 ? input.value.substring(afterComma) : '';
    input.value = before + (before && !before.endsWith(',') ? '' : '') + tag + after;
    dd.classList.add('hidden');
    input.focus();
  }

  input.addEventListener('input', update);
  input.addEventListener('focus', update);
  input.addEventListener('blur', function() { setTimeout(function(){ dd.classList.add('hidden'); }, 150); });
  dd.addEventListener('mousedown', function(e) {
    var item = e.target.closest('.tag-ac-item');
    if (item) { e.preventDefault(); pickTag(item.getAttribute('data-tag')); }
  });
}

function renderGrid() {
  var grid = document.getElementById('link-grid');
  var noResults = document.getElementById('no-results');

  if (filteredLinx.length === 0) {
    grid.innerHTML = '';
    noResults.className = '';
    document.getElementById('link-count').textContent = '0 items';
    return;
  }
  noResults.className = 'hidden';

  var html = '';
  for (var i = 0; i < filteredLinx.length; i++) {
    var item = filteredLinx[i];
    if (item.type === 'document') {
      html += renderDocumentLinx(item);
      continue;
    }
    if (item.type !== 'link') {
      html += renderPersonLinx(item);
      continue;
    }
    var linxStyle = item.color ? ' style="border-left:3px solid ' + item.color + '"' : '';
    html += '<div class="linx-item" data-id="' + item.id + '"' + linxStyle + ' tabindex="0" oncontextmenu="showCtxMenu(event,' + item.id + ')" ondblclick="dblClickLinx(' + item.id + ')">'
      + '<span class="linx-shortname">' + escHtml(item.shortName) + '</span>'
      + '<div class="linx-url" title="' + escHtml(item.destinationURL) + '">' + escHtml(item.destinationURL) + '</div>';
    if (item.description) {
      html += '<div class="linx-desc">' + escHtml(item.description) + '</div>';
    }
    html += renderTags(item.tags);
    html += '<div class="linx-meta">';
    if (item.owner) {
      html += '<span class="linx-owner">' + escHtml(item.owner) + '</span>';
    } else {
      html += '<span></span>';
    }
    html += '<span class="linx-clicks">' + (item.clickCount || 0) + ' clicks</span>';
    html += '</div></div>';
  }
  grid.innerHTML = html;
  document.getElementById('link-count').textContent = filteredLinx.length + ' item' + (filteredLinx.length !== 1 ? 's' : '');
}

function loadLinx() {
  fetch('/api/linx').then(function(r) { return r.json(); }).then(function(items) {
    allLinx = items || [];
    filterLinx();
  }).catch(function(e) {
    showToast('Failed to load data: ' + e.message, 'error');
  });
}

// Theme
function applyTheme(name) {
  var t = themes[name]; if (!t) return;
  var root = document.documentElement.style;
  var keys = Object.keys(t);
  for (var i = 0; i < keys.length; i++) {
    root.setProperty(keys[i], t[keys[i]]);
  }
  if (!t['--font']) {
    root.setProperty('--font', "'Segoe UI', system-ui, -apple-system, sans-serif");
  }
  if (name === 'ibm-3278') {
    document.body.setAttribute('data-square', '');
  } else {
    document.body.removeAttribute('data-square');
  }
  if (!_restoring) fetch('/api/settings', {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({key: 'theme', value: name})
  }).catch(function(){});
}

// View mode
function setViewMode(mode) {
  viewMode = mode;
  var grid = document.getElementById('link-grid');
  if (mode === 'list') {
    grid.classList.add('list-mode');
  } else {
    grid.classList.remove('list-mode');
  }
  document.getElementById('viewGrid').className = 'view-btn' + (mode === 'grid' ? ' view-active' : '');
  document.getElementById('viewList').className = 'view-btn' + (mode === 'list' ? ' view-active' : '');
  if (!_restoring) fetch('/api/settings', {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({key: 'viewMode', value: mode})
  }).catch(function(){});
}

// Context menu
function showCtxMenu(event, linxId) {
  event.preventDefault();
  event.stopPropagation();
  ctxTarget = null;
  for (var i = 0; i < allLinx.length; i++) {
    if (allLinx[i].id === linxId) { ctxTarget = allLinx[i]; break; }
  }
  var editable = ctxTarget && userCanEdit(ctxTarget);
  document.getElementById('ctxView').style.display = editable ? 'none' : '';
  document.getElementById('ctxEdit').style.display = editable ? '' : 'none';
  var canDel = editable && hasPerm('delete');
  document.getElementById('ctxSep').style.display = canDel ? '' : 'none';
  document.getElementById('ctxDelete').style.display = canDel ? '' : 'none';
  var menu = document.getElementById('ctx-menu');
  menu.style.left = event.clientX + 'px';
  menu.style.top = event.clientY + 'px';
  menu.classList.add('visible');
  requestAnimationFrame(function() {
    var rect = menu.getBoundingClientRect();
    if (rect.right > window.innerWidth) menu.style.left = (window.innerWidth - rect.width - 4) + 'px';
    if (rect.bottom > window.innerHeight) menu.style.top = (window.innerHeight - rect.height - 4) + 'px';
  });
}

function hideCtxMenu() {
  document.getElementById('ctx-menu').classList.remove('visible');
}

function toggleGearMenu(event) {
  event.stopPropagation();
  var menu = document.getElementById('gear-menu');
  var btn = document.getElementById('gearBtn');
  if (menu.classList.contains('visible')) {
    hideGearMenu();
    return;
  }
  var rect = btn.getBoundingClientRect();
  menu.style.left = rect.left + 'px';
  menu.style.top = '-9999px';
  menu.classList.add('visible');
  var mh = menu.offsetHeight;
  menu.style.top = (rect.top - mh - 4) + 'px';
}

function hideGearMenu() {
  document.getElementById('gear-menu').classList.remove('visible');
}

function dblClickLinx(linxId) {
  var lnx = null;
  for (var i = 0; i < allLinx.length; i++) {
    if (allLinx[i].id === linxId) { lnx = allLinx[i]; break; }
  }
  if (!lnx) return;
  window.open('/' + lnx.shortName, '_blank', 'noopener');
}

function ctxView() {
  hideCtxMenu();
  if (!ctxTarget) return;
  showEditModal(ctxTarget, true);
}

function ctxEdit() {
  hideCtxMenu();
  if (!ctxTarget) return;
  showEditModal(ctxTarget);
}

function ctxDelete() {
  hideCtxMenu();
  if (!ctxTarget) return;
  showDeleteModal(ctxTarget);
}

// Color picker
function pickColor(prefix, color) {
  document.getElementById(prefix + 'Color').value = color;
  var swatches = document.getElementById(prefix + 'ColorPicker').querySelectorAll('.color-swatch');
  for (var i = 0; i < swatches.length; i++) {
    if (swatches[i].getAttribute('data-color') === color) {
      swatches[i].classList.add('selected');
    } else {
      swatches[i].classList.remove('selected');
    }
  }
}

// New Linx Modal
function showNewLinxModal() {
  document.getElementById('newType').value = 'link';
  document.getElementById('newShortName').value = '';
  document.getElementById('newDestURL').value = '';
  document.getElementById('newDescription').value = '';
  document.getElementById('newOwner').value = _currentUserLogin;
  document.getElementById('newFirstName').value = '';
  document.getElementById('newLastName').value = '';
  document.getElementById('newTitle').value = '';
  document.getElementById('newEmail').value = '';
  document.getElementById('newPhone').value = '';
  document.getElementById('newWebLink').value = '';
  document.getElementById('newCalLink').value = '';
  document.getElementById('newXLink').value = '';
  document.getElementById('newLinkedInLink').value = '';
  document.getElementById('newDocTitle').value = '';
  document.getElementById('newDocFormat').value = 'text/markdown';
  document.getElementById('newDocContent').value = '';
  document.getElementById('newDocFile').value = '';
  document.getElementById('newDocOwner').value = _currentUserLogin;
  document.getElementById('newTags').value = '';
  pickColor('new', '');
  toggleNewLinxType();
  updateShortNamePreview('new');
  document.getElementById('newOverlay').classList.remove('hidden');
  document.getElementById('newShortName').focus();
}

function closeNewLinxModal() {
  document.getElementById('newOverlay').classList.add('hidden');
  document.getElementById('newShortNameHints').classList.add('hidden');
}

function toggleNewLinxType() {
  var t = document.getElementById('newType').value;
  document.getElementById('newLinkFields').classList.add('hidden');
  document.getElementById('newPersonFields').classList.add('hidden');
  document.getElementById('newDocumentFields').classList.add('hidden');
  if (t === 'link') {
    document.getElementById('newLinkFields').classList.remove('hidden');
    document.getElementById('btnGenerate').style.display = '';
  } else if (t === 'document') {
    document.getElementById('newDocumentFields').classList.remove('hidden');
    document.getElementById('btnGenerate').style.display = '';
  } else {
    document.getElementById('newPersonFields').classList.remove('hidden');
    document.getElementById('btnGenerate').style.display = 'none';
  }
}

// Format phone number as 810-454-6786 (digits only, auto-dashes)
function formatPhone(input) {
  var digits = input.value.replace(/\D/g, '').slice(0, 10);
  if (digits.length > 6) input.value = digits.slice(0,3) + '-' + digits.slice(3,6) + '-' + digits.slice(6);
  else if (digits.length > 3) input.value = digits.slice(0,3) + '-' + digits.slice(3);
  else input.value = digits;
}
// Generate random short code (bit.ly style)
function generateShortCode() {
  var chars = 'abcdefghjkmnpqrstuvwxyz23456789';
  var code = '';
  for (var i = 0; i < 6; i++) code += chars.charAt(Math.floor(Math.random() * chars.length));
  var input = document.getElementById('newShortName');
  input.value = code;
  updateShortNameHints('newShortName', 'newShortNameHints');
  updateShortNamePreview('new');
  input.focus();
}

// Live short name preview in label
function updateShortNamePreview(prefix) {
  var val = document.getElementById(prefix + 'ShortName').value.trim();
  var preview = document.getElementById(prefix + 'ShortNamePreview');
  var live = preview.querySelector('.shortname-live');
  live.textContent = val;
  if (val) {
    var proto = window.location.protocol + '//';
    var host = _shortHostname || window.location.host;
    preview.href = proto + host + '/' + encodeURIComponent(val);
    preview.classList.add('has-name');
  } else {
    preview.removeAttribute('href');
    preview.classList.remove('has-name');
  }
}

// Short name hints — show matching existing names as you type
function updateShortNameHints(inputId, hintsId) {
  var val = document.getElementById(inputId).value.trim().toLowerCase();
  var box = document.getElementById(hintsId);
  if (!val) { box.classList.add('hidden'); box.innerHTML = ''; return; }
  var matches = [];
  for (var i = 0; i < allLinx.length; i++) {
    var sn = allLinx[i].shortName.toLowerCase();
    if (sn.indexOf(val) !== -1) matches.push(allLinx[i].shortName);
  }
  if (matches.length === 0) { box.classList.add('hidden'); box.innerHTML = ''; return; }
  matches.sort();
  var html = '';
  for (var i = 0; i < matches.length && i < 8; i++) {
    var cls = matches[i].toLowerCase() === val ? ' class="taken"' : '';
    html += '<div' + cls + '>' + matches[i] + (matches[i].toLowerCase() === val ? ' (taken)' : '') + '</div>';
  }
  if (matches.length > 8) html += '<div>... ' + (matches.length - 8) + ' more</div>';
  box.innerHTML = html;
  box.classList.remove('hidden');
}

document.addEventListener('input', function(e) {
  if (e.target.id === 'newPhone' || e.target.id === 'editPhone') formatPhone(e.target);
  if (e.target.id === 'newShortName') updateShortNameHints('newShortName', 'newShortNameHints');
  if (e.target.id === 'editShortName') updateShortNameHints('editShortName', 'editShortNameHints');
});

function saveNewLinx() {
  var linxType = document.getElementById('newType').value;
  var shortName = document.getElementById('newShortName').value.trim();
  var color = document.getElementById('newColor').value;

  if (linxType === 'link') {
    var destURL = document.getElementById('newDestURL').value.trim();
    if (!shortName || !destURL) {
      showToast('Short Name and Destination URL are required', 'error');
      return;
    }
    var data = {
      type: 'link', shortName: shortName, destinationURL: destURL,
      description: document.getElementById('newDescription').value.trim(),
      owner: document.getElementById('newOwner').value.trim(),
      color: color
    };
  } else if (linxType === 'document') {
    var docTitle = document.getElementById('newDocTitle').value.trim();
    var docContent = document.getElementById('newDocContent').value;
    var docFormat = document.getElementById('newDocFormat').value;
    if (!shortName || !docTitle) {
      showToast('Short Name and Title are required', 'error');
      return;
    }
    if (!docContent) {
      showToast('Document content is required', 'error');
      return;
    }
    var data = {
      type: 'document', shortName: shortName, description: docTitle,
      owner: document.getElementById('newDocOwner').value.trim(),
      color: color
    };
    var _docContent = docContent;
    var _docFormat = docFormat;
  } else {
    var firstName = document.getElementById('newFirstName').value.trim();
    if (!shortName || !firstName) {
      showToast('Short Name and First Name are required', 'error');
      return;
    }
    var data = {
      type: linxType, shortName: shortName, firstName: firstName,
      lastName: document.getElementById('newLastName').value.trim(),
      title: document.getElementById('newTitle').value.trim(),
      email: document.getElementById('newEmail').value.trim(),
      phone: document.getElementById('newPhone').value.trim(),
      webLink: document.getElementById('newWebLink').value.trim(),
      calLink: document.getElementById('newCalLink').value.trim(),
      xLink: document.getElementById('newXLink').value.trim(),
      linkedInLink: document.getElementById('newLinkedInLink').value.trim(),
      color: color
    };
  }
  data.tags = document.getElementById('newTags').value.trim();

  fetch('/api/linx', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(data)
  }).then(function(r) {
    if (!r.ok) return r.text().then(function(t) { throw new Error(t); });
    return r.json();
  }).then(function(created) {
    if (linxType === 'document') {
      return fetch('/api/linx/' + created.id + '/document', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({content: _docContent, mime: _docFormat})
      }).then(function(r) {
        if (!r.ok) return r.text().then(function(t) { throw new Error(t); });
      });
    }
  }).then(function() {
    closeNewLinxModal();
    loadLinx();
    showToast('Linx created', 'success');
  }).catch(function(e) {
    showToast(e.message || 'Failed to create linx', 'error');
  });
}

// Unified Edit Linx Modal
function showEditModal(lnx, readonly) {
  editingLinxId = lnx.id;
  editingLinxType = lnx.type;
  document.getElementById('editShortName').value = lnx.shortName;

  document.getElementById('editLinkFields').classList.add('hidden');
  document.getElementById('editPersonFields').classList.add('hidden');
  document.getElementById('editDocumentFields').classList.add('hidden');

  if (lnx.type === 'link') {
    document.getElementById('editModalTitle').textContent = readonly ? 'Linx Info' : 'Edit Linx';
    document.getElementById('editLinkFields').classList.remove('hidden');
    document.getElementById('editDestURL').value = lnx.destinationURL;
    document.getElementById('editDescription').value = lnx.description || '';
    document.getElementById('editOwner').value = lnx.owner || '';
  } else if (lnx.type === 'document') {
    document.getElementById('editModalTitle').textContent = readonly ? 'Document Info' : 'Edit Document';
    document.getElementById('editDocumentFields').classList.remove('hidden');
    document.getElementById('editDocTitle').value = lnx.description || '';
    document.getElementById('editDocOwner').value = lnx.owner || '';
    document.getElementById('editDocFormat').value = lnx.documentMime || 'text/markdown';
    document.getElementById('editDocContent').value = '';
    document.getElementById('editDocFile').value = '';
    fetch('/api/linx/' + lnx.id + '/document').then(function(r) {
      if (r.ok) return r.text();
      return '';
    }).then(function(text) {
      document.getElementById('editDocContent').value = text || '';
    }).catch(function() {});
  } else {
    document.getElementById('editModalTitle').textContent = readonly ? 'Linx Info' : ('Edit ' + typeBadge(lnx.type));
    document.getElementById('editPersonFields').classList.remove('hidden');
    document.getElementById('editFirstName').value = lnx.firstName;
    document.getElementById('editLastName').value = lnx.lastName || '';
    document.getElementById('editTitle').value = lnx.title || '';
    document.getElementById('editEmail').value = lnx.email || '';
    document.getElementById('editPhone').value = lnx.phone || '';
    document.getElementById('editWebLink').value = lnx.webLink || '';
    document.getElementById('editCalLink').value = lnx.calLink || '';
    document.getElementById('editXLink').value = lnx.xLink || '';
    document.getElementById('editLinkedInLink').value = lnx.linkedInLink || '';
    var preview = document.getElementById('editAvatarPreview');
    if (lnx.avatarMime) {
      preview.innerHTML = '<img src="/api/linx/' + lnx.id + '/avatar" alt="avatar" />';
    } else {
      preview.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="8" r="4"/><path d="M20 21a8 8 0 0 0-16 0"/></svg>';
    }
    document.getElementById('editAvatarFile').value = '';
  }
  pickColor('edit', lnx.color || '');
  document.getElementById('editTags').value = lnx.tags || '';
  document.getElementById('editCreated').textContent = formatTime(lnx.dateCreated);
  document.getElementById('editLastClicked').textContent = formatTime(lnx.lastClicked);
  document.getElementById('editClicks').textContent = String(lnx.clickCount || 0);
  updateShortNamePreview('edit');

  // Toggle readonly mode
  var fields = document.querySelectorAll('#editOverlay input, #editOverlay select, #editOverlay textarea');
  for (var i = 0; i < fields.length; i++) {
    fields[i].disabled = !!readonly;
  }
  var swatches = document.querySelectorAll('#editColorPicker .color-swatch');
  for (var i = 0; i < swatches.length; i++) {
    swatches[i].style.pointerEvents = readonly ? 'none' : '';
    swatches[i].style.opacity = readonly ? '0.5' : '';
  }
  document.getElementById('editSaveBtn').style.display = readonly ? 'none' : '';
  document.getElementById('editCancelBtn').textContent = readonly ? 'Close' : 'Cancel';

  document.getElementById('editOverlay').classList.remove('hidden');
  if (!readonly) document.getElementById('editShortName').focus();
}

document.getElementById('editAvatarFile').addEventListener('change', function() {
  var file = this.files && this.files[0];
  if (!file) return;
  var reader = new FileReader();
  reader.onload = function(e) {
    document.getElementById('editAvatarPreview').innerHTML = '<img src="' + e.target.result + '" alt="avatar" />';
  };
  reader.readAsDataURL(file);
});

function handleDocFileUpload(fileInputId, formatSelectId, contentTextareaId) {
  document.getElementById(fileInputId).addEventListener('change', function() {
    var file = this.files && this.files[0];
    if (!file) return;
    var reader = new FileReader();
    reader.onload = function(e) {
      document.getElementById(contentTextareaId).value = e.target.result;
    };
    reader.readAsText(file);
    var name = file.name.toLowerCase();
    var sel = document.getElementById(formatSelectId);
    if (name.endsWith('.md') || name.endsWith('.markdown')) sel.value = 'text/markdown';
    else if (name.endsWith('.html') || name.endsWith('.htm')) sel.value = 'text/html';
    else sel.value = 'text/plain';
  });
}
handleDocFileUpload('newDocFile', 'newDocFormat', 'newDocContent');
handleDocFileUpload('editDocFile', 'editDocFormat', 'editDocContent');

function closeEditModal() {
  document.getElementById('editOverlay').classList.add('hidden');
  document.getElementById('editShortNameHints').classList.add('hidden');
  editingLinxId = null;
  editingLinxType = null;
}

function saveEditLinx() {
  if (!editingLinxId) return;
  var shortName = document.getElementById('editShortName').value.trim();
  var data = { type: editingLinxType, shortName: shortName, color: document.getElementById('editColor').value, tags: document.getElementById('editTags').value.trim() };

  if (editingLinxType === 'link') {
    data.destinationURL = document.getElementById('editDestURL').value.trim();
    data.description = document.getElementById('editDescription').value.trim();
    data.owner = document.getElementById('editOwner').value.trim();
    if (!shortName || !data.destinationURL) {
      showToast('Short Name and Destination URL are required', 'error');
      return;
    }
  } else if (editingLinxType === 'document') {
    data.description = document.getElementById('editDocTitle').value.trim();
    data.owner = document.getElementById('editDocOwner').value.trim();
    if (!shortName || !data.description) {
      showToast('Short Name and Title are required', 'error');
      return;
    }
    var _editDocContent = document.getElementById('editDocContent').value;
    var _editDocFormat = document.getElementById('editDocFormat').value;
  } else {
    data.firstName = document.getElementById('editFirstName').value.trim();
    data.lastName = document.getElementById('editLastName').value.trim();
    data.title = document.getElementById('editTitle').value.trim();
    data.email = document.getElementById('editEmail').value.trim();
    data.phone = document.getElementById('editPhone').value.trim();
    data.webLink = document.getElementById('editWebLink').value.trim();
    data.calLink = document.getElementById('editCalLink').value.trim();
    data.xLink = document.getElementById('editXLink').value.trim();
    data.linkedInLink = document.getElementById('editLinkedInLink').value.trim();
    if (!shortName || !data.firstName) {
      showToast('Short Name and First Name are required', 'error');
      return;
    }
  }

  var _editId = editingLinxId;
  var _editType = editingLinxType;
  fetch('/api/linx/' + _editId, {
    method: 'PUT',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(data)
  }).then(function(r) {
    if (!r.ok) return r.text().then(function(t) { throw new Error(t); });
    return r.json();
  }).then(function() {
    if (_editType === 'document') {
      return fetch('/api/linx/' + _editId + '/document', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({content: _editDocContent, mime: _editDocFormat})
      }).then(function(r) {
        if (!r.ok) return r.text().then(function(t) { throw new Error(t); });
      });
    } else if (_editType !== 'link') {
      var fileInput = document.getElementById('editAvatarFile');
      if (fileInput.files && fileInput.files[0]) {
        var fd = new FormData();
        fd.append('avatar', fileInput.files[0]);
        return fetch('/api/linx/' + _editId + '/avatar', {
          method: 'POST', body: fd
        });
      }
    }
  }).then(function() {
    closeEditModal();
    loadLinx();
    showToast('Linx updated', 'success');
  }).catch(function(e) {
    showToast(e.message || 'Failed to update', 'error');
  });
}

// Delete Modal
function showDeleteModal(lnx) {
  deletingLinxId = lnx.id;
  document.getElementById('deleteShortName').textContent = lnx.shortName;
  var subtitle = lnx.type === 'link' ? lnx.destinationURL : lnx.type === 'document' ? (lnx.description || 'Document') : (lnx.firstName + ' ' + lnx.lastName);
  document.getElementById('deleteSubtitle').textContent = subtitle;
  document.getElementById('deleteModalTitle').textContent = 'Delete ' + (lnx.type === 'link' ? 'Link' : typeBadge(lnx.type));
  document.getElementById('deleteOverlay').classList.remove('hidden');
}

function closeDeleteModal() {
  document.getElementById('deleteOverlay').classList.add('hidden');
  deletingLinxId = null;
}

function openDestHelp() {
  document.getElementById('destHelpOverlay').classList.remove('hidden');
}
function closeDestHelp() {
  document.getElementById('destHelpOverlay').classList.add('hidden');
}

function confirmDelete() {
  if (!deletingLinxId) return;
  fetch('/api/linx/' + deletingLinxId, { method: 'DELETE' }).then(function(r) {
    if (!r.ok) return r.text().then(function(t) { throw new Error(t); });
    return r.json();
  }).then(function() {
    closeDeleteModal();
    loadLinx();
    showToast('Deleted', 'success');
  }).catch(function(e) {
    showToast(e.message || 'Failed to delete', 'error');
  });
}

// Toast
function showToast(msg, type) {
  var t = document.getElementById('toast');
  t.textContent = msg;
  t.className = 'toast ' + (type || 'success') + ' visible';
  setTimeout(function() { t.classList.remove('visible'); }, 2500);
}

// Focus trap for modals
function trapFocus(overlay, e) {
  var focusable = overlay.querySelectorAll('input:not([type=hidden]):not([disabled]), select:not([disabled]), textarea:not([disabled]), button:not([style*="display:none"]):not([style*="display: none"])');
  if (focusable.length === 0) return;
  var first = focusable[0], last = focusable[focusable.length - 1];
  if (e.shiftKey) {
    if (document.activeElement === first || !overlay.querySelector('.modal-box').contains(document.activeElement)) {
      e.preventDefault(); last.focus();
    }
  } else {
    if (document.activeElement === last || !overlay.querySelector('.modal-box').contains(document.activeElement)) {
      e.preventDefault(); first.focus();
    }
  }
}

// Keyboard shortcuts
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') {
    if (!document.getElementById('destHelpOverlay').classList.contains('hidden')) { closeDestHelp(); return; }
    if (!document.getElementById('newOverlay').classList.contains('hidden')) closeNewLinxModal();
    else if (!document.getElementById('editOverlay').classList.contains('hidden')) closeEditModal();
    else if (!document.getElementById('deleteOverlay').classList.contains('hidden')) closeDeleteModal();
    hideCtxMenu();
    hideGearMenu();
  }
  if (e.key === 'F1') {
    e.preventDefault();
    window.open('/.help', '_blank');
  }
  // Ctrl+S saves the active modal
  if ((e.ctrlKey || e.metaKey) && e.key === 's') {
    var newOv = document.getElementById('newOverlay');
    var editOv = document.getElementById('editOverlay');
    if (!newOv.classList.contains('hidden')) {
      e.preventDefault(); saveNewLinx();
    } else if (!editOv.classList.contains('hidden') && document.getElementById('editSaveBtn').style.display !== 'none') {
      e.preventDefault(); saveEditLinx();
    }
  }
});

document.addEventListener('click', function() { hideCtxMenu(); hideGearMenu(); });
document.getElementById('searchInput').addEventListener('input', filterLinx);
document.getElementById('searchInput').addEventListener('keydown', function(e) {
  if (e.key === 'Enter' && filteredLinx.length === 1) {
    window.open('/' + encodeURIComponent(filteredLinx[0].shortName), '_blank');
    this.value = '';
    filterLinx();
  }
});

// Enter on focused linx opens the link
document.addEventListener('keydown', function(e) {
  if (e.key !== 'Enter') return;
  var el = document.activeElement;
  if (!el || !el.classList.contains('linx-item')) return;
  var linxId = parseInt(el.getAttribute('data-id'), 10);
  if (linxId) dblClickLinx(linxId);
});

// Arrow key navigation in the linx grid
document.addEventListener('keydown', function(e) {
  if (['ArrowUp','ArrowDown','ArrowLeft','ArrowRight'].indexOf(e.key) === -1) return;
  var el = document.activeElement;
  if (!el || !el.classList.contains('linx-item')) return;
  var els = Array.prototype.slice.call(document.querySelectorAll('#link-grid .linx-item'));
  if (els.length === 0) return;
  var idx = els.indexOf(el);
  if (idx === -1) return;
  // Detect columns by counting items that share the same top offset as the first
  var cols = 1;
  var firstTop = els[0].getBoundingClientRect().top;
  for (var i = 1; i < els.length; i++) {
    if (Math.abs(els[i].getBoundingClientRect().top - firstTop) < 2) cols++;
    else break;
  }
  var next = -1;
  if (e.key === 'ArrowRight') next = idx + 1;
  else if (e.key === 'ArrowLeft') next = idx - 1;
  else if (e.key === 'ArrowDown') next = idx + cols;
  else if (e.key === 'ArrowUp') next = idx - cols;
  if (next >= 0 && next < els.length) {
    e.preventDefault();
    els[next].focus();
  }
});

// Trap Tab inside app: cycle through search input + linx
document.addEventListener('keydown', function(e) {
  if (e.key !== 'Tab') return;
  // Trap focus inside open modals
  var modals = document.querySelectorAll('.modal-overlay');
  for (var m = 0; m < modals.length; m++) {
    if (!modals[m].classList.contains('hidden')) { trapFocus(modals[m], e); return; }
  }
  var search = document.getElementById('searchInput');
  var els = Array.prototype.slice.call(document.querySelectorAll('#link-grid .linx-item'));
  if (els.length === 0) { e.preventDefault(); search.focus(); return; }
  var active = document.activeElement;
  var idx = els.indexOf(active);
  e.preventDefault();
  if (e.shiftKey) {
    if (active === search) { els[els.length - 1].focus(); }
    else if (idx <= 0) { search.focus(); }
    else { els[idx - 1].focus(); }
  } else {
    if (active === search) { els[0].focus(); }
    else if (idx >= els.length - 1) { search.focus(); }
    else { els[idx + 1].focus(); }
  }
});

// Init
var _restoring = true;
var _currentUserLogin = '';
var _tsMode = false;
var _isAdmin = false;
var _adminMode = false;
var _localhostAdmin = false;
var _shortHostname = '';
var _userPerms = ['add','update','delete'];
function hasPerm(p) {
  return _userPerms.indexOf('*') >= 0 || _userPerms.indexOf(p) >= 0;
}
function userCanEdit(lnx) {
  if (_localhostAdmin) return true;
  if (!hasPerm('update')) return false;
  if (!_tsMode) return true;
  if (_isAdmin && _adminMode) return true;
  if (!lnx.owner) return true;
  return lnx.owner === _currentUserLogin;
}
function toggleAdminMode(on) {
  _adminMode = on;
  fetch('/api/settings', {
    method: 'PUT', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({key: 'adminMode', value: on ? 'true' : 'false'})
  }).catch(function(){});
  renderGrid();
}
(function init() {
  // Fetch current user identity
  fetch('/api/whoami').then(function(r) {
    return r.ok ? r.json() : null;
  }).then(function(data) {
    if (data && data.login) _currentUserLogin = data.login;
    if (data && data.tsMode) _tsMode = true;
    if (data && data.localhostAdmin) _localhostAdmin = true;
    if (data && data.perms) _userPerms = data.perms;
    if (!hasPerm('add')) document.getElementById('addBtn').style.display = 'none';
    if (data && data.isAdmin && !data.localhostAdmin) {
      _isAdmin = true;
      document.getElementById('adminToggle').classList.remove('hidden');
      // Restore admin mode setting
      fetch('/api/settings?key=adminMode').then(function(r) {
        return r.ok ? r.json() : null;
      }).then(function(s) {
        if (s && s.value === 'true') {
          _adminMode = true;
          document.getElementById('adminCheck').checked = true;
        }
      }).catch(function(){});
    }
    if (data && data.hostname) {
      var prefix = (data.tsMode && data.tsHostname) ? data.tsHostname : data.hostname;
      _shortHostname = prefix;
      var spans = document.querySelectorAll('.hostname-prefix');
      for (var i = 0; i < spans.length; i++) spans[i].textContent = prefix + '/';
    }
  }).catch(function(){});

  // Restore theme
  fetch('/api/settings?key=theme').then(function(r) {
    return r.ok ? r.json() : null;
  }).then(function(data) {
    if (data && data.value && themes[data.value]) {
      document.getElementById('themeSelect').value = data.value;
      applyTheme(data.value);
    }
  }).catch(function(){});

  // Restore view mode
  fetch('/api/settings?key=viewMode').then(function(r) {
    return r.ok ? r.json() : null;
  }).then(function(data) {
    if (data && data.value && (data.value === 'grid' || data.value === 'list')) {
      setViewMode(data.value);
    }
  }).catch(function(){});

  // Restore sort mode
  fetch('/api/settings?key=sortMode').then(function(r) {
    return r.ok ? r.json() : null;
  }).then(function(data) {
    if (data && data.value && (data.value === 'az' || data.value === 'popular' || data.value === 'recent')) {
      sortMode = data.value;
      var btns = document.querySelectorAll('.sort-btn');
      for (var i = 0; i < btns.length; i++) {
        btns[i].className = 'sort-btn' + (btns[i].getAttribute('data-sort') === data.value ? ' sort-active' : '');
      }
    }
  }).catch(function(){});

  setupTagAutocomplete('newTags');
  setupTagAutocomplete('editTags');

  loadLinx();
  setTimeout(function() { _restoring = false; }, 500);

  // Auto-open New Linx modal when redirected from /.addlinx
  if (new URLSearchParams(window.location.search).get('new') === '1') {
    history.replaceState(null, '', '/');
    setTimeout(showNewLinxModal, 200);
  }
})();
</script>
</body>
</html>` + ""
