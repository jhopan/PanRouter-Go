package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"panrouter/proxy/internal/config"
)

// Port of upstream src/lib/tunnel/tailscale/tailscale.js installTailscale, and
// of the SSE route that drives it
// (src/app/api/tunnel/tailscale-install/route.js).
//
// The observable contract is the route's: progress lines while the binary is
// installed, then the login URL the operator has to visit (or none, on a
// platform whose installer starts the service logged out). The mechanism is the
// same one upstream uses per platform — Homebrew on macOS when present, the
// signed package when not, the vendor install script on Linux, the MSI through
// an elevated msiexec on Windows — so a failure reads the same as it does
// upstream ("Wrong sudo password", "Download failed", "msiexec failed (code N)").

const (
	tailscaleDarwinPkgURL = "https://pkgs.tailscale.com/stable/tailscale-latest.pkg"
	tailscaleLinuxScript  = "https://tailscale.com/install.sh"
	tailscaleWindowsMSI   = "https://pkgs.tailscale.com/stable/tailscale-setup-latest-amd64.msi"
)

var (
	// installPlatform and the command/download seams exist so a test can drive
	// every branch without installing anything or reaching the network.
	installPlatform = runtime.GOOS
	installExecFn   = func(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		return cmd.CombinedOutput()
	}
	installDownloadFn = downloadFile
	installHasBrewFn  = func() bool {
		if installPlatform != "darwin" {
			return false
		}
		_, err := exec.LookPath("brew")
		return err == nil
	}
	installHTTPClient = &http.Client{Timeout: 5 * time.Minute}
	// tailscaleLoginURLPattern is upstream's auth-URL matcher.
	tailscaleLoginURLPattern = regexp.MustCompile(`https://login\.tailscale\.com/a/[a-zA-Z0-9]+`)
	// tailscaleDaemonReadyWait bounds the poll after a daemon start, matching
	// upstream's 3s "wait for socket ready".
	tailscaleDaemonReadyWait = 3 * time.Second
	// tailscaleLoginWait bounds the auth-URL poll (upstream: 15s).
	tailscaleLoginWait = 15 * time.Second
)

// TailscaleInstallOptions is one install request.
type TailscaleInstallOptions struct {
	// SudoPassword is used to install the package and start a root daemon on
	// macOS/Linux. It is empty on Windows (UAC is used) and on a Homebrew-only
	// macOS install.
	SudoPassword string
	// Hostname is the tailnet hostname the node logs in with (upstream passes
	// the tunnel shortId).
	Hostname string
	// Progress receives install log lines, one per line, as they happen.
	Progress func(string)
}

func (o TailscaleInstallOptions) log(msg string) {
	if o.Progress != nil {
		o.Progress(msg)
	}
}

// downloadFile fetches url into dest, refusing an empty body so a proxy error
// page is never mistaken for an installer.
func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := installHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	file, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	n, err := io.Copy(file, resp.Body)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("download failed: empty body")
	}
	return nil
}

// InstallTailscale installs the CLI when it is missing, brings the daemon up
// and starts the login flow. The returned URL is "" when the node is already
// logged in or when the platform's installer does not log in (Windows).
func InstallTailscale(ctx context.Context, opts TailscaleInstallOptions) (string, error) {
	if findTailscaleBin() == "" {
		if err := installTailscaleBinary(ctx, opts); err != nil {
			return "", err
		}
	}

	if installPlatform == "windows" {
		// Windows: the MSI registers a service. Start it and wait for the
		// backend to leave NoState, exactly as upstream does, but there is no
		// login step here — the desktop app owns it.
		opts.log("Starting Tailscale service...")
		_, _ = installExecFn(ctx, "", "net", "start", "Tailscale")
		waitTailscaleBackend(ctx, opts)
		opts.log("Installation complete.")
		return "", nil
	}

	opts.log("Starting daemon...")
	ensureTailscaleDaemon(ctx, opts)

	opts.log("Logging in...")
	return startTailscaleLogin(ctx, opts)
}

// installTailscaleBinary runs the platform installer. A missing installer is an
// error the route reports verbatim, not a silent success.
func installTailscaleBinary(ctx context.Context, opts TailscaleInstallOptions) error {
	switch installPlatform {
	case "windows":
		return installTailscaleWindows(ctx, opts)
	case "darwin":
		if installHasBrewFn() {
			return installTailscaleBrew(ctx, opts)
		}
		return installTailscaleMacPkg(ctx, opts)
	default:
		return installTailscaleLinux(ctx, opts)
	}
}

func installTailscaleBrew(ctx context.Context, opts TailscaleInstallOptions) error {
	opts.log("Installing via Homebrew...")
	out, err := installExecFn(ctx, "", "brew", "install", "tailscale")
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			opts.log(line)
		}
	}
	if err != nil {
		return fmt.Errorf("brew install failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func installTailscaleMacPkg(ctx context.Context, opts TailscaleInstallOptions) error {
	pkgPath := filepath.Join(os.TempDir(), "tailscale.pkg")
	defer func() { _ = os.Remove(pkgPath) }()

	opts.log("Downloading Tailscale package...")
	if err := installDownloadFn(ctx, tailscaleDarwinPkgURL, pkgPath); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	opts.log("Installing package...")
	out, err := installExecFn(ctx, opts.SudoPassword+"\n", "sudo", "-S", "installer", "-pkg", pkgPath, "-target", "/")
	if err != nil {
		if isWrongSudoPassword(string(out)) {
			return errors.New("Wrong sudo password")
		}
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}

func installTailscaleLinux(ctx context.Context, opts TailscaleInstallOptions) error {
	// A newline in the password would become a second command on sudo's stdin.
	if strings.Contains(opts.SudoPassword, "\n") {
		return errors.New("Invalid sudo password")
	}

	opts.log("Downloading install script...")
	scriptPath := filepath.Join(os.TempDir(), fmt.Sprintf("tailscale-install-%d.sh", time.Now().UnixNano()))
	defer func() { _ = os.Remove(scriptPath) }()
	if err := installDownloadFn(ctx, tailscaleLinuxScript, scriptPath); err != nil {
		return fmt.Errorf("failed to download install script: %w", err)
	}
	if err := os.Chmod(scriptPath, 0o700); err != nil {
		return fmt.Errorf("failed to write install script: %w", err)
	}

	opts.log("Running install script...")
	out, err := installExecFn(ctx, opts.SudoPassword+"\n", "sudo", "-S", "sh", scriptPath)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			opts.log(line)
		}
	}
	if err != nil {
		if isWrongSudoPassword(string(out)) {
			return errors.New("Wrong sudo password")
		}
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}

func installTailscaleWindows(ctx context.Context, opts TailscaleInstallOptions) error {
	msiPath := filepath.Join(os.TempDir(), "tailscale-setup.msi")
	defer func() { _ = os.Remove(msiPath) }()

	opts.log("Downloading Tailscale installer...")
	if err := installDownloadFn(ctx, tailscaleWindowsMSI, msiPath); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	opts.log("Installing Tailscale (UAC prompt may appear)...")
	args := fmt.Sprintf("'/i','%s','TS_NOLAUNCH=true','/quiet','/norestart'", msiPath)
	out, err := installExecFn(ctx, "", "powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Start-Process msiexec -ArgumentList "+args+" -Verb RunAs -Wait")
	if err != nil {
		if strings.TrimSpace(string(out)) != "" {
			opts.log(strings.TrimSpace(string(out)))
		}
		return fmt.Errorf("msiexec failed: %s", strings.TrimSpace(string(out)))
	}

	opts.log("Verifying installation...")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if findTailscaleBin() != "" {
			opts.log("Installation complete.")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("installation finished but tailscale.exe not found")
}

// isWrongSudoPassword recognises sudo's own refusals.
func isWrongSudoPassword(output string) bool {
	return strings.Contains(output, "incorrect password") || strings.Contains(output, "Sorry")
}

// waitTailscaleBackend polls `tailscale status --json` until the backend leaves
// NoState, with the same ~10s budget the upstream Windows path uses.
func waitTailscaleBackend(ctx context.Context, opts TailscaleInstallOptions) {
	bin := findTailscaleBin()
	if bin == "" {
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := probeTailscale(ctx, bin); err == nil && st != nil && st.BackendState != "" && st.BackendState != "NoState" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ensureTailscaleDaemon starts tailscaled when the CLI cannot reach one, then
// waits briefly for it to answer. A daemon that is already up is left alone:
// killing a working root daemon to replace it with a userspace one would take
// down a working funnel.
func ensureTailscaleDaemon(ctx context.Context, opts TailscaleInstallOptions) {
	bin := findTailscaleBin()
	if bin == "" {
		return
	}
	if _, err := probeTailscale(ctx, bin); err == nil {
		return
	}
	stateDir := filepath.Join(config.ResolveDataDir(), "tailscale")
	_ = os.MkdirAll(stateDir, 0o755)
	socket := filepath.Join(stateDir, "tailscaled.sock")
	statedir := filepath.Join(stateDir, "tailscaled.state")

	args := []string{"--socket=" + socket, "--statedir=" + statedir}
	var cmd *exec.Cmd
	if opts.SudoPassword != "" {
		args = append([]string{"-S", "tailscaled"}, args...)
		cmd = exec.Command("sudo", args...)
		cmd.Stdin = strings.NewReader(opts.SudoPassword + "\n")
	} else {
		args = append(args, "--tun=userspace-networking")
		cmd = exec.Command("tailscaled", args...)
	}
	if err := cmd.Start(); err != nil {
		return
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(tailscaleDaemonReadyWait)
	for time.Now().Before(deadline) {
		if _, err := probeTailscale(ctx, bin); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// startTailscaleLogin runs `tailscale up` and returns the auth URL. An already
// logged-in node answers "" (upstream resolves { alreadyLoggedIn: true }).
func startTailscaleLogin(ctx context.Context, opts TailscaleInstallOptions) (string, error) {
	bin := findTailscaleBin()
	if bin == "" {
		return "", errNoTailscale
	}
	if st, err := probeTailscale(ctx, bin); err == nil && st != nil && st.BackendState == "Running" {
		return "", nil
	}

	args := []string{"up", "--accept-routes"}
	if opts.Hostname != "" {
		args = append(args, "--hostname="+opts.Hostname)
	}

	// Run `up` in the background: it prints the login URL and then waits for the
	// human, so reading its output to completion would block the SSE stream.
	// stdout and stderr share one mutex-guarded buffer because the polling loop
	// below reads it while os/exec writes to it.
	cmd := exec.Command(bin, args...)
	var output syncedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() { _ = cmd.Process.Release() }()

	deadline := time.Now().Add(tailscaleLoginWait)
	for time.Now().Before(deadline) {
		if match := tailscaleLoginURLPattern.FindString(output.String()); match != "" {
			return match, nil
		}
		if st, err := probeTailscale(ctx, bin); err == nil && st != nil && st.AuthURL != "" {
			return st.AuthURL, nil
		}
		if st, err := probeTailscale(ctx, bin); err == nil && st != nil && st.BackendState == "Running" {
			return "", nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	if match := tailscaleLoginURLPattern.FindString(output.String()); match != "" {
		return match, nil
	}
	return "", fmt.Errorf("tailscale up timed out without auth URL")
}

// syncedBuffer is a concurrency-safe output sink: os/exec writes to it from its
// own goroutine while the login poll reads it.
type syncedBuffer struct {
	mu   sync.Mutex
	data strings.Builder
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}
