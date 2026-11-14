// Package dbbackup ports src/shared/services/dbBackup.js: automatic
// snapshots of the live SQLite database pushed to a private GitHub repo.
//
// Why: the box holds every provider connection with its OAuth token. If the
// disk dies, or a release corrupts the schema, every account is gone — the
// token is the only way back in. A daily snapshot pushed OFF the box is the
// safety net.
//
// The backup is the raw SQLite file. It is taken with VACUUM INTO so the copy
// is consistent while the server keeps writing: a plain file copy of a live
// SQLite database silently misses everything still sitting in the write-ahead
// log, and the restored DB would be missing recent accounts.
//
// Design notes, carried over from upstream:
//   - The GitHub token NEVER leaves the server. It lives in the settings row
//     next to the dashboard password hash, and the settings read strips it.
//   - The target repo MUST be private. A public repo would publish every OAuth
//     token on the box, so the push is refused unless GitHub reports the repo
//     as private.
//   - The push is a plain HTTPS git remote with the token embedded. No extra
//     tooling is needed; git is the only dependency.
package dbbackup

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"panrouter/proxy/internal/db"
	"panrouter/proxy/internal/log"
)

const (
	// DefaultIntervalHours is the interval used when the operator has not
	// chosen one: once a day. Values below 1 hour are refused — the snapshot is
	// a full copy of the database and copying it every few minutes is pointless
	// churn on the disk and in the repo.
	DefaultIntervalHours = 24

	// MaxIntervalHours bounds the other end (one week).
	MaxIntervalHours = 24 * 7

	// KeepSnapshots is how many snapshots the repo keeps.
	KeepSnapshots = 30

	// tickInterval is how often the scheduler wakes up to check whether a
	// backup is due.
	tickInterval = 5 * time.Minute
)

// repoNamePattern is upstream's /^[\w.-]+$/: the repository name has to be a
// plain name, never a path fragment or a URL.
var repoNamePattern = regexp.MustCompile(`^[\w.-]+$`)

// RepoNameLooksValid reports whether a stored repository name is acceptable.
func RepoNameLooksValid(repo string) bool { return repoNamePattern.MatchString(repo) }

// Config is the stored automatic-backup configuration (settings.dbBackup).
type Config struct {
	Enabled         bool
	IntervalHours   int
	GithubUser      string
	GithubRepo      string
	GithubRepoOwner string
	GithubToken     string
}

// ConfigFromSettings reads the dbBackup block out of the raw settings map.
//
// A missing block reads as "disabled, defaults": every field is derived from
// the stored JSON alone, so a config written by the Next dashboard and one
// written here parse the same way.
func ConfigFromSettings(raw map[string]any) Config {
	cfg := Config{IntervalHours: DefaultIntervalHours}
	block, _ := raw["dbBackup"].(map[string]any)
	if block == nil {
		return cfg
	}
	cfg.Enabled = block["enabled"] == true
	cfg.IntervalHours = ResolveIntervalHours(block["intervalHours"])
	cfg.GithubUser = stringField(block, "githubUser")
	cfg.GithubRepo = stringField(block, "githubRepo")
	cfg.GithubRepoOwner = stringField(block, "githubRepoOwner")
	cfg.GithubToken = stringField(block, "githubToken")
	return cfg
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

// Reported resolves the owner/repository pair the dashboard displays.
//
// Older configs stored "owner/name" in githubRepo; newer ones store the bare
// name plus githubRepoOwner. Parsing here means both shapes report an owner
// rather than one of them showing an empty column.
func (c Config) Reported() (owner, repo string) {
	ref := ParseRepoRef(c.GithubRepo, c.GithubUser)
	if c.GithubRepoOwner != "" {
		return c.GithubRepoOwner, c.GithubRepo
	}
	if ref != nil {
		return ref.Owner, ref.Repo
	}
	return "", ""
}

// ResolveIntervalHours maps the stored value onto the accepted band. Anything
// unset, non-numeric, zero or negative means the default; anything finite is
// clamped to [1, 168].
func ResolveIntervalHours(raw any) int {
	hours := 0.0
	ok := false
	switch v := raw.(type) {
	case float64:
		hours, ok = v, true
	case float32:
		hours, ok = float64(v), true
	case int:
		hours, ok = float64(v), true
	case int64:
		hours, ok = float64(v), true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			hours, ok = f, true
		}
	}
	if !ok || hours <= 0 || hours != hours {
		return DefaultIntervalHours
	}
	if hours < 1 {
		return 1
	}
	if hours > MaxIntervalHours {
		return MaxIntervalHours
	}
	return int(hours)
}

// RepoRef is the owner/repository pair parsed out of whatever the operator
// pasted into the repository field.
type RepoRef struct {
	Owner string
	Repo  string
}

// ParseRepoRef reads the accepted shapes:
//
//	https://github.com/jhopan12/AutoBuy.git
//	https://github.com/jhopan12/AutoBuy
//	git@github.com:jhopan12/AutoBuy.git
//	github.com/jhopan12/AutoBuy
//	jhopan12/AutoBuy
//	AutoBuy                       (owner comes from the username field)
//
// It returns nil when the input is not a repository reference at all, and nil
// when there is no owner to be had (a bare name with no username fallback).
func ParseRepoRef(raw, fallbackOwner string) *RepoRef {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil
	}

	rest := text
	if m := sshRepoPattern.FindStringSubmatch(rest); m != nil {
		// git@github.com:owner/repo -> github.com/owner/repo
		rest = m[1] + "/" + m[2]
	} else {
		// Strip a scheme and any host prefix, so the remainder is owner/repo.
		if loc := schemePrefix.FindStringIndex(rest); loc != nil {
			rest = rest[loc[1]:]
		}
		if m := hostAndPath.FindStringSubmatch(rest); m != nil && hostPrefixPattern.MatchString(m[1]) {
			rest = m[2]
		}
	}

	rest = gitSuffixPattern.ReplaceAllString(rest, "")
	rest = strings.Trim(rest, "/")

	parts := make([]string, 0, 4)
	for _, p := range strings.Split(rest, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	// A leading "owner" segment only counts when there are two of them.
	if len(parts) >= 2 {
		return &RepoRef{Owner: parts[len(parts)-2], Repo: parts[len(parts)-1]}
	}
	owner := strings.TrimSpace(fallbackOwner)
	if owner == "" {
		return nil
	}
	return &RepoRef{Owner: owner, Repo: parts[0]}
}

var (
	sshRepoPattern    = regexp.MustCompile(`^(?:ssh://)?git@([^:]+):(.+)$`)
	schemePrefix      = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://`)
	hostAndPath       = regexp.MustCompile(`^([^/]+)/(.+)$`)
	hostPrefixPattern = regexp.MustCompile(`^(?i)(github\.com$|github\.)`)
	gitSuffixPattern  = regexp.MustCompile(`(?i)\.git$`)
)

// PushRequest is one snapshot waiting to be committed upstream.
type PushRequest struct {
	Owner    string
	Repo     string
	Token    string
	FileName string
	Content  []byte
}

// Result reports one backup cycle. Reason carries the upstream message
// verbatim, because the dashboard prints it as-is.
type Result struct {
	OK       bool
	Reason   string
	FileName string
	Bytes    int
}

// Service runs backups. Every side effect is a field so a test can replace the
// network and the git binary; nil means the production implementation.
type Service struct {
	Repo *db.Repo

	// Now overrides the clock in tests. It decides the snapshot file name.
	Now func() time.Time
	// CheckRepoPrivate asks GitHub whether the repo is private.
	CheckRepoPrivate func(ctx context.Context, owner, repo, token string) error
	// Snapshot writes a consistent copy of the live database to dest.
	Snapshot func(ctx context.Context, dest string) error
	// Push commits and pushes one snapshot, returning the bytes written.
	Push func(ctx context.Context, req PushRequest) (int, error)

	running   atomic.Bool
	lastRunAt atomic.Int64

	mu   sync.Mutex
	stop chan struct{}
}

// New builds the production service over repo.
func New(repo *db.Repo) *Service {
	return &Service{
		Repo:             repo,
		Now:              time.Now,
		CheckRepoPrivate: AssertRepoIsPrivate,
		Snapshot:         vacuumInto(repo),
		Push:             PushSnapshot,
	}
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Run performs one backup cycle: read the config, verify the repo is private,
// snapshot the database, push it.
//
// force skips the "enabled" check — that is what the dashboard's "Back up now"
// button does, so an operator can prove the setup works without waiting for
// the night.
func (s *Service) Run(ctx context.Context, force bool) Result {
	if s == nil || s.Repo == nil {
		return Result{Reason: "The backup store is not available"}
	}
	if !s.running.CompareAndSwap(false, true) {
		return Result{Reason: "A backup is already running"}
	}
	defer s.running.Store(false)

	raw, err := s.Repo.GetSettingsRaw()
	if err != nil {
		return Result{Reason: "Cannot read settings: " + err.Error()}
	}
	cfg := ConfigFromSettings(raw)
	if !force && !cfg.Enabled {
		return Result{Reason: "Automatic backup is disabled"}
	}
	if cfg.GithubUser == "" || cfg.GithubToken == "" || cfg.GithubRepo == "" {
		return Result{Reason: "GitHub username, token, or repo is not configured"}
	}

	// The repository field may hold a full URL, an ssh remote, "owner/repo", or
	// a bare repo name. Parse it so the owner comes from the URL when the
	// operator pasted one.
	ref := ParseRepoRef(cfg.GithubRepo, cfg.GithubUser)
	if ref == nil {
		return Result{Reason: fmt.Sprintf("Cannot read the repository from %q", cfg.GithubRepo)}
	}

	if err := s.CheckRepoPrivate(ctx, ref.Owner, ref.Repo, cfg.GithubToken); err != nil {
		return Result{Reason: err.Error()}
	}

	stamp := s.now().Format("2006-01-02")
	fileName := "panrouter-db-" + stamp + ".sqlite"
	work, err := os.MkdirTemp("", "panrouter-dbbackup-")
	if err != nil {
		return Result{Reason: err.Error()}
	}
	defer func() { _ = os.RemoveAll(work) }()

	tmpPath := filepath.Join(work, fileName)
	if err := s.Snapshot(ctx, tmpPath); err != nil {
		return Result{Reason: err.Error()}
	}
	content, err := os.ReadFile(tmpPath)
	if err != nil {
		return Result{Reason: err.Error()}
	}
	bytesWritten, err := s.Push(ctx, PushRequest{
		Owner:    ref.Owner,
		Repo:     ref.Repo,
		Token:    cfg.GithubToken,
		FileName: fileName,
		Content:  content,
	})
	if err != nil {
		return Result{Reason: err.Error()}
	}
	s.lastRunAt.Store(s.now().Unix())
	log.Info("dbbackup", "pushed snapshot", "file", fileName, "bytes", bytesWritten, "repo", ref.Owner+"/"+ref.Repo)
	return Result{OK: true, FileName: fileName, Bytes: bytesWritten}
}

// vacuumInto snapshots the live database through the adapter's own VACUUM
// INTO. The alternative — copying the file — is not safe while the server is
// writing: recent rows still live in the -wal sidecar and would be missing
// from the copy.
func vacuumInto(repo *db.Repo) func(ctx context.Context, dest string) error {
	return func(ctx context.Context, dest string) error {
		if repo == nil || repo.RawDB() == nil {
			return errors.New("database not available")
		}
		// VACUUM INTO refuses to overwrite, and a name we just made is free.
		if _, err := repo.RawDB().ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
			return fmt.Errorf("snapshot database into %s: %w", filepath.Base(dest), err)
		}
		return nil
	}
}

// githubHTTPClient bounds every GitHub read. The API is not on the request
// path, but a hung call would hold the "backup running" flag forever.
var githubHTTPClient = &http.Client{Timeout: 20 * time.Second}

// AssertRepoIsPrivate asks GitHub whether the repo is private. A public repo
// would publish every token in the database, so this is a hard gate — not a
// warning.
func AssertRepoIsPrivate(ctx context.Context, owner, repo, token string) error {
	url := "https://api.github.com/repos/" + owner + "/" + repo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "panrouter-db-backup")

	resp, err := githubHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub: cannot read repo %s/%s: %w", owner, repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GitHub: cannot read repo %s/%s (HTTP %d) — check the token and the repo name", owner, repo, resp.StatusCode)
	}
	var data struct {
		Private *bool `json:"private"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return fmt.Errorf("GitHub: cannot read repo %s/%s: %w", owner, repo, err)
	}
	if data.Private == nil || !*data.Private {
		return fmt.Errorf("GitHub: repo %s/%s is NOT private — refusing to upload the database", owner, repo)
	}
	return nil
}

// snapshotNamePattern is the file name shape pushSnapshot prunes: only files
// this feature wrote are candidates, so a README or a workflow file in the
// repo is never touched.
var snapshotNamePattern = regexp.MustCompile(`^panrouter-db-\d{4}-\d{2}-\d{2}\.sqlite$`)

// PushSnapshot clones the repo, writes the snapshot, prunes old snapshots,
// commits and pushes. The repo is cloned into a temp directory that is removed
// afterwards, so nothing survives between runs.
func PushSnapshot(ctx context.Context, req PushRequest) (int, error) {
	work, err := os.MkdirTemp("", "panrouter-dbbackup-push-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(work) }()

	remote := fmt.Sprintf("https://%s:%s@github.com/%s/%s.git", req.Owner, req.Token, req.Owner, req.Repo)
	if out, err := runGit(ctx, "", "clone", "--depth", "1", remote, work); err != nil {
		return 0, fmt.Errorf("git clone failed: %s", outcome(out, err))
	}
	if err := os.WriteFile(filepath.Join(work, req.FileName), req.Content, 0o600); err != nil {
		return 0, err
	}

	// Keep the repo bounded: drop the oldest snapshots past the retention.
	entries, err := os.ReadDir(work)
	if err != nil {
		return 0, err
	}
	var existing []string
	for _, e := range entries {
		if !e.IsDir() && snapshotNamePattern.MatchString(e.Name()) {
			existing = append(existing, e.Name())
		}
	}
	sort.Strings(existing)
	overflow := len(existing) - (KeepSnapshots - 1)
	for i := range overflow {
		_ = os.Remove(filepath.Join(work, existing[i]))
	}

	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := runGitEnv(ctx, work, env, "add", "-A"); err != nil {
		return 0, fmt.Errorf("git add failed: %s", outcome(out, err))
	}
	// --allow-empty: an unchanged day still records that a backup ran.
	if out, err := runGitEnv(ctx, work, env, "commit", "--allow-empty", "-m", "db backup "+time.Now().Format("2006-01-02")); err != nil {
		return 0, fmt.Errorf("git commit failed: %s", outcome(out, err))
	}
	if out, err := runGitEnv(ctx, work, env, "push", "origin", "HEAD"); err != nil {
		return 0, fmt.Errorf("git push failed: %s", outcome(out, err))
	}
	return len(req.Content), nil
}

func outcome(out string, err error) string {
	if strings.TrimSpace(out) != "" {
		return strings.TrimSpace(out)
	}
	return err.Error()
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	return runGitEnv(ctx, dir, append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), args...)
}

func runGitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// Start begins the periodic tick. It returns immediately when the scheduler is
// already running, which is every call after the first.
func (s *Service) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return
	}
	stop := make(chan struct{})
	s.stop = stop
	log.Info("dbbackup", "scheduler started")
	go func() {
		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.tick()
			}
		}
	}()
}

// Stop halts the periodic tick. The in-flight cycle, if any, finishes.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop == nil {
		return
	}
	close(s.stop)
	s.stop = nil
	log.Info("dbbackup", "scheduler stopped")
}

// Configure starts or stops the scheduler to match the stored settings. It is
// called when the operator saves the config, so a switch takes effect without
// a restart.
func (s *Service) Configure() {
	if s == nil || s.Repo == nil {
		return
	}
	raw, err := s.Repo.GetSettingsRaw()
	if err != nil {
		return
	}
	if ConfigFromSettings(raw).Enabled {
		s.Start()
		return
	}
	s.Stop()
}

// tick runs the backup when the configured interval has elapsed.
func (s *Service) tick() {
	if s == nil || s.Repo == nil {
		return
	}
	raw, err := s.Repo.GetSettingsRaw()
	if err != nil {
		return
	}
	cfg := ConfigFromSettings(raw)
	if !cfg.Enabled {
		return
	}
	interval := time.Duration(cfg.IntervalHours) * time.Hour
	last := s.lastRunAt.Load()
	if last > 0 && s.now().Sub(time.Unix(last, 0)) < interval {
		return
	}
	s.Run(context.Background(), false)
}
