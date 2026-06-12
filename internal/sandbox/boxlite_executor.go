package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qs3c/bkclaw/internal/workspace"
)

// Boxlite REST sandbox provider — talks the OpenAPI spec at
// https://github.com/boxlite-ai/boxlite/blob/main/openapi/rest-sandbox-open-api.yaml
//
// Auth is a static API key sent as `Authorization: Bearer <apikey>`.
// Earlier revisions did an OAuth2 client_credentials exchange against
// /oauth/tokens — that endpoint was removed upstream; the key the
// operator pastes is now the bearer token directly. BoxliteClientID is
// retained on the config struct for back-compat (existing admin rows
// keep working) but it isn't sent anywhere anymore.
//
// Lifecycle:
//   POST   /{prefix}/boxes          create box (configured)
//   POST   /{prefix}/boxes/{id}/start  start VM (we do this eagerly so
//                                      Hydrate has a running target)
//   PUT    /{prefix}/boxes/{id}/files?path=/  application/x-tar bulk upload
//   POST   /{prefix}/boxes/{id}/exec  → returns {execution_id}
//   GET    /{prefix}/boxes/{id}/executions/{exec_id}/attach
//          → WebSocket bidirectional, binary frames are [channel:u8][bytes]
//            (0x01 stdout, 0x02 stderr), text frame {"type":"exit","exit_code":N}
//            on completion followed by a normal close.
//   DELETE /{prefix}/boxes/{id}?force=true

const (
	// BoxLite Cloud dev environment — the only public endpoint verified
	// end-to-end (OAuth + createBox + attach). The OpenAPI servers
	// stanza advertises `https://api.boxlite.ai/v1`, but that host sits
	// behind Cloudflare Access and rejects ordinary client_credentials
	// with a 403 HTML wall. When BoxLite ships a publicly reachable
	// prod endpoint, update this default — until then, operators on
	// prod tenants must explicitly set their URL in the admin UI.
	defaultBoxliteURL      = "https://api.dev.boxlite.ai/api/v1"
	defaultBoxliteClientID = "default"
	defaultBoxlitePrefix   = "default"
	defaultBoxliteImage    = "thinkany/bkclaw-sandbox:latest"
)

// BoxliteExecutor implements Executor against a remote Boxlite REST API.
type BoxliteExecutor struct {
	baseURL string // already trimmed of trailing slash
	prefix  string
	// clientID is the legacy OAuth2 client_id, retained on the struct so
	// older config rows that set it don't break the constructor. Unused
	// after the apikey-as-bearer switch.
	clientID string
	apiKey   string
	image    string
	timeout  time.Duration

	client *http.Client

	mu    sync.Mutex
	boxID string

	// hydration sources — same shape as E2BExecutor uses, so recreate()
	// can rebuild /skills + /workspace from scratch when the box is gone.
	skillDirs []string
	workspace workspace.Store
	agentID   string
	projectID string
	sessionID string
}

func newBoxliteExecutor(ctx context.Context, baseURL, prefix, clientID, apiKey, image string, timeout time.Duration) (*BoxliteExecutor, error) {
	if baseURL == "" {
		baseURL = defaultBoxliteURL
	}
	if prefix == "" {
		prefix = defaultBoxlitePrefix
	}
	if clientID == "" {
		clientID = defaultBoxliteClientID
	}
	if image == "" {
		image = defaultBoxliteImage
	}
	if apiKey == "" {
		return nil, fmt.Errorf("boxlite: apikey is required")
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	// No global http.Client.Timeout: exec round-trips can run for
	// minutes (long-running builds, image gen, etc.). We bound at the
	// request level via context.WithTimeout — same pattern E2BExecutor
	// settled on after the 60s-streaming-cut bug.
	e := &BoxliteExecutor{
		baseURL:  strings.TrimRight(baseURL, "/"),
		prefix:   prefix,
		clientID: clientID,
		apiKey:   apiKey,
		image:    image,
		timeout:  timeout,
		client:   &http.Client{},
	}

	if err := e.createBox(ctx); err != nil {
		return nil, fmt.Errorf("boxlite create box: %w", err)
	}
	if err := e.startBox(ctx); err != nil {
		// Best-effort cleanup if start fails so we don't leak a stuck
		// configured-but-never-started box on the server.
		_ = e.Close()
		return nil, fmt.Errorf("boxlite start box: %w", err)
	}
	return e, nil
}

// authHeader returns "Bearer <apikey>". The apikey IS the bearer token
// in the new auth scheme — no exchange, no cache, no expiry handling.
// Returns a (string, error) shape so callers (which previously needed
// to handle token-refresh failures) don't have to be rewritten; the
// error result is always nil today.
func (e *BoxliteExecutor) authHeader(_ context.Context) (string, error) {
	return "Bearer " + e.apiKey, nil
}

func (e *BoxliteExecutor) prefixPath(suffix string) string {
	return fmt.Sprintf("%s/%s%s", e.baseURL, e.prefix, suffix)
}

func (e *BoxliteExecutor) createBox(ctx context.Context) error {
	body, _ := json.Marshal(map[string]interface{}{
		"image": e.image,
		// auto_remove keeps the server from accumulating stopped
		// boxes when our Close() racing the network drops the
		// explicit DELETE — they self-collect when stopped.
		"auto_remove": true,
	})
	createCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(createCtx, "POST", e.prefixPath("/boxes"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	auth, err := e.authHeader(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var box struct {
		BoxID string `json:"box_id"`
	}
	if err := json.Unmarshal(respBody, &box); err != nil {
		return fmt.Errorf("decode box: %w", err)
	}
	if box.BoxID == "" {
		return fmt.Errorf("empty box_id in response: %s", string(respBody))
	}
	e.mu.Lock()
	e.boxID = box.BoxID
	e.mu.Unlock()
	slog.Info("boxlite box created", "boxID", box.BoxID, "image", e.image)
	return nil
}

func (e *BoxliteExecutor) startBox(ctx context.Context) error {
	startCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(startCtx, "POST", e.prefixPath("/boxes/"+e.boxID+"/start"), nil)
	if err != nil {
		return err
	}
	auth, err := e.authHeader(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// SetHydrationSources records the inputs Hydrate() should pull from. Same
// shape as E2BExecutor — set after construction so recreate() can replay.
func (e *BoxliteExecutor) SetHydrationSources(skillDirs []string, ws workspace.Store, agentID, projectID, sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.skillDirs = append(e.skillDirs[:0], skillDirs...)
	e.workspace = ws
	e.agentID = agentID
	e.projectID = projectID
	e.sessionID = sessionID
}

// Hydrate packs /skills and /workspace into a single tar and pushes it via
// `PUT /files?path=/`. Unlike the E2B path we don't need base64 + exec
// shenanigans — Boxlite's Files API takes raw tar bytes and extracts them
// at the destination directly.
func (e *BoxliteExecutor) Hydrate(ctx context.Context) error {
	bundle := newPlainTarBundle()

	skillCount := 0
	skillFileCount := 0
	seen := make(map[string]bool)
	for _, dir := range e.skillDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if seen[name] {
				continue
			}
			seen[name] = true
			n, err := bundle.addLocalDir(filepath.Join(dir, name), "skills/"+name)
			if err != nil {
				slog.Warn("boxlite hydrate: skill tar", "skill", name, "error", err)
				continue
			}
			skillCount++
			skillFileCount += n
		}
	}

	workspaceCount := 0
	if e.workspace != nil {
		listProject := e.projectID
		listSession := e.sessionID
		if e.projectID != "" {
			listSession = ""
		}
		objs, err := e.workspace.List(ctx, e.agentID, listProject, listSession)
		if err != nil {
			slog.Warn("boxlite hydrate: workspace list", "agent", e.agentID, "error", err)
		} else {
			for _, obj := range objs {
				rc, err := e.workspace.Get(ctx, e.agentID, listProject, listSession, obj.Path)
				if err != nil {
					slog.Warn("boxlite hydrate: workspace get", "path", obj.Path, "error", err)
					continue
				}
				data, rerr := io.ReadAll(rc)
				rc.Close()
				if rerr != nil {
					slog.Warn("boxlite hydrate: workspace read", "path", obj.Path, "error", rerr)
					continue
				}
				rel := strings.TrimPrefix(obj.Path, "/")
				if err := bundle.addBytes("workspace/"+rel, data, 0o644, obj.ModTime); err != nil {
					slog.Warn("boxlite hydrate: workspace tar", "path", obj.Path, "error", err)
					continue
				}
				workspaceCount++
			}
		}
	}

	// Always ensure the parent dirs exist on the remote side, even when
	// the tar would be empty — the agent's first write_file would
	// otherwise fail with ENOENT.
	if err := bundle.ensureDir("skills/"); err != nil {
		return fmt.Errorf("tar skills dir: %w", err)
	}
	if err := bundle.ensureDir("workspace/"); err != nil {
		return fmt.Errorf("tar workspace dir: %w", err)
	}
	if err := bundle.close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}

	// BoxLite Files API note: despite the OpenAPI spec advertising
	// "Uploads a tar archive and extracts it at the specified path",
	// the dev cloud's PUT /files does NOT extract. Empirically:
	//   - path = a file path → writes the request body verbatim to
	//     that file (parents are created automatically). 204.
	//   - path = an existing directory + Content-Type x-tar →
	//     stores the raw tar as `boxlite-upload-<rand>.tar` inside it.
	//     Still 204 — silently wrong for our hydrate purposes.
	// We work around by:
	//   1. PUT the tar to a deterministic file path `/tmp/hydrate.tar`
	//   2. exec `tar -xf /tmp/hydrate.tar -C /` to actually unpack
	//   3. remove the staging file so /tmp stays clean
	// One upload + one exec is still cheaper than per-file PUTs when
	// a skill bundle has dozens of files each.
	const stagingPath = "/tmp/fc-hydrate.tar"
	uploadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	uploadURL := e.prefixPath("/boxes/"+e.boxID+"/files") +
		"?path=" + url.QueryEscape(stagingPath) + "&overwrite=true"
	req, err := http.NewRequestWithContext(uploadCtx, "PUT", uploadURL, bytes.NewReader(bundle.buf.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	auth, err := e.authHeader(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("upload tar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload tar HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Unpack via exec. tar -C / + bundle paths like "skills/..." land
	// content at /skills/... — matches what the agent's tools expect
	// (python /skills/<name>/main.py) and what the Docker backend
	// gives via bind mount. rm afterwards keeps /tmp tidy across
	// recreate() cycles.
	extractCmd := fmt.Sprintf("tar -xf %s -C / && rm -f %s", stagingPath, stagingPath)
	if _, err := e.execOnce(ctx, extractCmd, 60*time.Second); err != nil {
		return fmt.Errorf("extract hydrate tar: %w", err)
	}

	slog.Info("boxlite sandbox hydrated",
		"boxID", e.boxID,
		"skills", skillCount,
		"skillFiles", skillFileCount,
		"workspaceFiles", workspaceCount,
		"tarBytes", bundle.buf.Len())
	return nil
}

// plainTarBundle is the boxlite variant of e2b's tarBundle: no gzip
// because the Files API expects application/x-tar. addLocalDir /
// addBytes / ensureDir mirror the e2b helper.
type plainTarBundle struct {
	buf       bytes.Buffer
	tw        *tar.Writer
	fileCount int
}

func newPlainTarBundle() *plainTarBundle {
	b := &plainTarBundle{}
	b.tw = tar.NewWriter(&b.buf)
	return b
}

func (b *plainTarBundle) addBytes(name string, data []byte, mode int64, modTime time.Time) error {
	if modTime.IsZero() {
		modTime = time.Now()
	}
	if err := b.tw.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(name, "/"),
		Mode:     mode,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
		ModTime:  modTime,
	}); err != nil {
		return err
	}
	if _, err := b.tw.Write(data); err != nil {
		return err
	}
	b.fileCount++
	return nil
}

func (b *plainTarBundle) ensureDir(name string) error {
	return b.tw.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(name, "/"),
		Mode:     0o755,
		Typeflag: tar.TypeDir,
		ModTime:  time.Now(),
	})
}

func (b *plainTarBundle) addLocalDir(localRoot, prefix string) (int, error) {
	count := 0
	err := filepath.Walk(localRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(localRoot, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		name := prefix + "/" + filepath.ToSlash(rel)
		if err := b.addBytes(name, data, int64(info.Mode().Perm()), info.ModTime()); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

func (b *plainTarBundle) close() error { return b.tw.Close() }

// Exec runs a shell command via the async exec + WebSocket attach pair.
// On 404 (box gone) we recreate and retry once — matches the E2B
// recreate-on-stale pattern.
func (e *BoxliteExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	wrapped := "cd /workspace && " + command
	out, err := e.execOnce(ctx, wrapped, timeout)
	if err != nil && isBoxliteGone(err) {
		if rerr := e.recreate(ctx); rerr != nil {
			return "", fmt.Errorf("recreate after stale box: %w (original: %v)", rerr, err)
		}
		return e.execOnce(ctx, wrapped, timeout)
	}
	return out, err
}

func isBoxliteGone(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "HTTP 404") || strings.Contains(s, "HTTP 502") || strings.Contains(s, "HTTP 410")
}

func (e *BoxliteExecutor) recreate(ctx context.Context) error {
	slog.Info("boxlite box gone, recreating", "oldBoxID", e.boxID)
	// Static apikey — no token refresh; just rebuild the box state.
	if err := e.createBox(ctx); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if err := e.startBox(ctx); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := e.Hydrate(ctx); err != nil {
		return fmt.Errorf("hydrate: %w", err)
	}
	return nil
}

// execOnce starts an execution and streams its output over the attach
// WebSocket until the server emits {"type":"exit",...} and closes.
func (e *BoxliteExecutor) execOnce(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	body, _ := json.Marshal(map[string]interface{}{
		"command":         "/bin/sh",
		"args":            []string{"-c", command},
		"timeout_seconds": timeout.Seconds(),
	})
	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()
	req, err := http.NewRequestWithContext(startCtx, "POST", e.prefixPath("/boxes/"+e.boxID+"/exec"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	auth, err := e.authHeader(ctx)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("boxlite exec start: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("boxlite exec start HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var ex struct {
		ExecutionID string `json:"execution_id"`
	}
	if err := json.Unmarshal(respBody, &ex); err != nil {
		return "", fmt.Errorf("decode exec start: %w (%s)", err, string(respBody))
	}
	if ex.ExecutionID == "" {
		return "", fmt.Errorf("empty execution_id: %s", string(respBody))
	}

	return e.attachAndDrain(ctx, ex.ExecutionID, timeout)
}

// attachAndDrain opens the attach WebSocket and reads frames until an
// exit message or the parent context's deadline. Binary frames are the
// channel-tagged stdout/stderr payloads; text frames are control JSON.
func (e *BoxliteExecutor) attachAndDrain(ctx context.Context, execID string, timeout time.Duration) (string, error) {
	wsURL, err := e.attachWebsocketURL(execID)
	if err != nil {
		return "", err
	}
	auth, err := e.authHeader(ctx)
	if err != nil {
		return "", err
	}
	hdr := http.Header{}
	hdr.Set("Authorization", auth)

	// Give the WS dial a short bound — if the upgrade is going to fail
	// (bad token, wrong exec_id, box not running) it'll fail fast.
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()
	dialer := websocket.DefaultDialer
	conn, dialResp, err := dialer.DialContext(dialCtx, wsURL, hdr)
	if err != nil {
		status := 0
		body := ""
		if dialResp != nil {
			status = dialResp.StatusCode
			b, _ := io.ReadAll(dialResp.Body)
			body = string(b)
			dialResp.Body.Close()
		}
		return "", fmt.Errorf("boxlite attach: %w (HTTP %d: %s)", err, status, body)
	}
	defer conn.Close()

	// Stream deadline: user-supplied tool timeout + 30s slack so the
	// server has room to flush trailing frames before our side gives up.
	streamCtx, cancelStream := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancelStream()
	conn.SetReadDeadline(time.Now().Add(timeout + 30*time.Second))

	// Reset the read deadline on every Pong so the keepalive (server
	// pings every 15s per the spec) keeps the connection alive across
	// long-running execs.
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(timeout + 30*time.Second))
		return nil
	})

	// Async-cancel: if streamCtx times out or parent ctx is canceled,
	// nudge the connection closed so the blocking ReadMessage returns.
	doneReading := make(chan struct{})
	go func() {
		select {
		case <-streamCtx.Done():
			_ = conn.Close()
		case <-doneReading:
		}
	}()
	defer close(doneReading)

	var stdout, stderr bytes.Buffer
	exitCode := -1
	exited := false

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if exited {
				break
			}
			// Normal close from the server after the exit text frame
			// arrives as a websocket.CloseError(1000). Treat as success
			// if we already saw the exit frame above; otherwise surface.
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				break
			}
			return combineOutput(&stdout, &stderr), fmt.Errorf("attach read: %w", err)
		}
		switch msgType {
		case websocket.BinaryMessage:
			if len(data) == 0 {
				continue
			}
			channel := data[0]
			payload := data[1:]
			switch channel {
			case 0x01:
				stdout.Write(payload)
			case 0x02:
				stderr.Write(payload)
			default:
				// Unknown channel; the spec only defines 1 and 2 today,
				// but a future server might add more. Drop silently
				// rather than corrupt the merged output.
			}
		case websocket.TextMessage:
			var ctrl struct {
				Type     string `json:"type"`
				ExitCode int    `json:"exit_code"`
				Message  string `json:"message"`
			}
			if json.Unmarshal(data, &ctrl) != nil {
				continue
			}
			switch ctrl.Type {
			case "exit":
				exitCode = ctrl.ExitCode
				exited = true
				// Server is about to close. Stop reading; the next
				// ReadMessage will see the close frame.
			case "error":
				// Non-fatal per the spec — connection stays open. Log
				// and keep reading so we still capture the exit frame.
				slog.Warn("boxlite attach error frame", "boxID", e.boxID, "exec", execID, "message", ctrl.Message)
			}
		}
		if exited {
			// Drain a single more frame attempt for the close to come
			// through, then exit on read err above. ReadMessage with a
			// short deadline keeps this loop bounded.
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		}
	}

	output := combineOutput(&stdout, &stderr)
	slog.Info("boxlite exec completed", "boxID", e.boxID, "exec", execID, "exitCode", exitCode, "exited", exited, "outputLen", len(output))
	if !exited {
		return output, fmt.Errorf("exec did not emit exit frame before deadline")
	}
	if exitCode != 0 {
		if output == "" {
			output = fmt.Sprintf("Process exited with code %d", exitCode)
		}
		return output, fmt.Errorf("exit code %d", exitCode)
	}
	return output, nil
}

func combineOutput(stdout, stderr *bytes.Buffer) string {
	out := stdout.String()
	if stderr.Len() > 0 {
		if out != "" {
			out += "\n"
		}
		out += stderr.String()
	}
	return strings.TrimSpace(out)
}

// attachWebsocketURL builds the wss:// URL for the attach endpoint by
// mirroring the http(s) base URL onto the ws(s) scheme.
func (e *BoxliteExecutor) attachWebsocketURL(execID string) (string, error) {
	u, err := url.Parse(e.baseURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported scheme %q in boxlite URL", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + e.prefix + "/boxes/" + e.boxID + "/executions/" + execID + "/attach"
	return u.String(), nil
}

func (e *BoxliteExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	return e.Exec(ctx, fmt.Sprintf("cat %s", shellQuote(path)), 30*time.Second)
}

func (e *BoxliteExecutor) WriteFile(ctx context.Context, filePath, content string) (string, error) {
	// BoxLite Files API quirk: when path points at a concrete file
	// path the request body is written verbatim and parent dirs are
	// auto-created (verified empirically — see Hydrate's note). We
	// used to do a heredoc-over-exec dance, which broke on content
	// containing the random marker and on binary content; PUT is
	// binary-safe and skips the shell entirely.
	uploadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	uploadURL := e.prefixPath("/boxes/"+e.boxID+"/files") +
		"?path=" + url.QueryEscape(filePath) + "&overwrite=true"
	req, err := http.NewRequestWithContext(uploadCtx, "PUT", uploadURL, strings.NewReader(content))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	auth, err := e.authHeader(ctx)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("boxlite write: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("boxlite write HTTP %d: %s", resp.StatusCode, string(body))
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), filePath), nil
}

func (e *BoxliteExecutor) ListDir(ctx context.Context, path string) (string, error) {
	return e.Exec(ctx, fmt.Sprintf("ls -la %s", shellQuote(path)), 10*time.Second)
}

// shellQuote single-quotes a path for safe inclusion in a shell command.
// shellQuote is declared in docker_executor.go and shared package-wide;
// boxlite re-used the same helper rather than redeclaring it.

// Backend returns "boxlite" — used by the per-exec log line so operators
// can confirm at a glance which provider handled a given tool call.
func (e *BoxliteExecutor) Backend() string { return "boxlite" }

// IsRemoteWorkspace marks this executor as cloud-hosted so the
// LifecyclePool runs SnapshotWorkspace after every exec. Same contract
// E2BExecutor honors.
func (e *BoxliteExecutor) IsRemoteWorkspace() {}

// SnapshotWorkspace downloads /workspace as a tar via the Files API and
// returns the (path → bytes) map the LifecyclePool needs to mirror
// sandbox-side writes back to the durable workspace.Store.
func (e *BoxliteExecutor) SnapshotWorkspace(ctx context.Context) (map[string][]byte, error) {
	dlCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	u := e.prefixPath("/boxes/"+e.boxID+"/files") + "?path=/workspace"
	req, err := http.NewRequestWithContext(dlCtx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	auth, err := e.authHeader(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("snapshot HTTP %d: %s", resp.StatusCode, string(body))
	}
	out := make(map[string][]byte)
	tr := tar.NewReader(resp.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, fmt.Errorf("snapshot tar read: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Server tars the requested directory's contents; the entry
		// names are relative to /workspace already in well-behaved
		// implementations, but be defensive and strip both forms.
		name := strings.TrimPrefix(hdr.Name, "./")
		name = strings.TrimPrefix(name, "/")
		name = strings.TrimPrefix(name, "workspace/")
		if name == "" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return out, fmt.Errorf("snapshot read %s: %w", name, err)
		}
		out[name] = data
	}
	return out, nil
}

func (e *BoxliteExecutor) Close() error {
	e.mu.Lock()
	boxID := e.boxID
	e.boxID = ""
	e.mu.Unlock()
	if boxID == "" {
		return nil
	}
	delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(delCtx, "DELETE", e.prefixPath("/boxes/"+boxID)+"?force=true", nil)
	if err != nil {
		return err
	}
	auth, err := e.authHeader(delCtx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	slog.Info("boxlite box closed", "boxID", boxID)
	return nil
}

// BoxliteExecutorPool manages per-(agent, project, session) Boxlite boxes.
type BoxliteExecutorPool struct {
	mu        sync.Mutex
	executors map[string]*BoxliteExecutor
	baseURL   string
	prefix    string
	clientID  string
	apiKey    string
	image     string
	home      string
	timeout   time.Duration
	workspace workspace.Store
}

// Backend on the pool mirrors BoxliteExecutor.Backend so the LifecyclePool
// can surface the provider identity without resolving a lazy executor.
func (p *BoxliteExecutorPool) Backend() string { return "boxlite" }

// NewBoxliteExecutorPool constructs a Boxlite-backed pool. Defaults match
// the public Boxlite Cloud — operators can override URL/prefix/clientID
// for self-hosted runners or staging environments.
func NewBoxliteExecutorPool(baseURL, prefix, clientID, apiKey, image, home string, timeout time.Duration) *BoxliteExecutorPool {
	if baseURL == "" {
		baseURL = defaultBoxliteURL
	}
	if prefix == "" {
		prefix = defaultBoxlitePrefix
	}
	if clientID == "" {
		clientID = defaultBoxliteClientID
	}
	if image == "" {
		image = defaultBoxliteImage
	}
	return &BoxliteExecutorPool{
		executors: make(map[string]*BoxliteExecutor),
		baseURL:   baseURL,
		prefix:    prefix,
		clientID:  clientID,
		apiKey:    apiKey,
		image:     image,
		home:      home,
		timeout:   timeout,
	}
}

// SetWorkspace plugs in the workspace.Store whose contents should be
// mirrored to /workspace on every fresh box. Mirrors E2BExecutorPool.
func (p *BoxliteExecutorPool) SetWorkspace(ws workspace.Store) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workspace = ws
}

func (p *BoxliteExecutorPool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := poolKey(agentID, projectID, sessionID)
	if ex, ok := p.executors[key]; ok {
		return ex, nil
	}
	ex, err := newBoxliteExecutor(ctx, p.baseURL, p.prefix, p.clientID, p.apiKey, p.image, p.timeout)
	if err != nil {
		return nil, err
	}
	ex.SetHydrationSources(skillDirsForAgent(p.home, agentID), p.workspace, agentID, projectID, sessionID)
	if err := ex.Hydrate(ctx); err != nil {
		_ = ex.Close()
		return nil, fmt.Errorf("boxlite hydrate: %w", err)
	}
	p.executors[key] = ex
	return ex, nil
}

func (p *BoxliteExecutorPool) Release(agentID, projectID, sessionID string) error {
	p.mu.Lock()
	key := poolKey(agentID, projectID, sessionID)
	ex, ok := p.executors[key]
	delete(p.executors, key)
	p.mu.Unlock()
	if ok {
		return ex.Close()
	}
	return nil
}

func (p *BoxliteExecutorPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ex := range p.executors {
		_ = ex.Close()
	}
	p.executors = make(map[string]*BoxliteExecutor)
}

var (
	_ Executor             = (*BoxliteExecutor)(nil)
	_ ExecutorPool         = (*BoxliteExecutorPool)(nil)
	_ WorkspaceSnapshotter = (*BoxliteExecutor)(nil)
	_ RemoteWorkspace      = (*BoxliteExecutor)(nil)
)
