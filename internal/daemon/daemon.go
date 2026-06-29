package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Paths 返回 ~/.bkcrab 下的守护进程目录路径。
func Paths() (pidFile, logFile, logDir string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", err
	}
	base := filepath.Join(home, ".bkcrab")
	logDir = filepath.Join(base, "logs")
	pidFile = filepath.Join(base, "bkcrab.pid")
	logFile = filepath.Join(logDir, "gateway.log")
	return
}

// Status 表示当前守护进程状态。
type Status struct {
	Running bool
	PID     int
	Uptime  time.Duration
}

// GetStatus 检查守护进程是否正在运行。
func GetStatus() (*Status, error) {
	pidFile, _, _, err := Paths()
	if err != nil {
		return nil, err
	}

	pid, err := readPID(pidFile)
	if err != nil {
		return &Status{Running: false}, nil
	}

	if !isProcessAlive(pid) {
		// 过期的 PID 文件
		os.Remove(pidFile)
		return &Status{Running: false}, nil
	}

	// 从 PID 文件的修改时间估算运行时间
	info, err := os.Stat(pidFile)
	var uptime time.Duration
	if err == nil {
		uptime = time.Since(info.ModTime())
	}

	return &Status{Running: true, PID: pid, Uptime: uptime}, nil
}

// Start 将网关作为后台守护进程启动，并支持自动重启。
func Start(port int) error {
	st, _ := GetStatus()
	if st != nil && st.Running {
		return fmt.Errorf("daemon already running (PID %d)", st.PID)
	}

	pidFile, logFile, logDir, err := Paths()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	// 查找自身二进制文件
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	bin, _ = filepath.EvalSymlinks(bin)

	// 打开日志文件用于 stdout/stderr
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	// 启动守护进程包装器进程
	args := []string{"daemon", "__run", "--port", strconv.Itoa(port)}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	setSysProcAttr(cmd) // 平台特定的分离

	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("start daemon: %w", err)
	}

	// 写入 PID 文件
	if err := writePID(pidFile, cmd.Process.Pid); err != nil {
		lf.Close()
		return fmt.Errorf("write PID file: %w", err)
	}

	lf.Close()

	fmt.Printf("Daemon started (PID %d)\n", cmd.Process.Pid)
	fmt.Printf("Logs: %s\n", logFile)
	return nil
}

// RunLoop 是守护进程包装器，在崩溃时自动重启网关。
// 由 'daemon __run' 内部调用。
func RunLoop(port int) error {
	pidFile, logFile, _, err := Paths()
	if err != nil {
		return err
	}

	// 写入我们自己的 PID（包装器）
	if err := writePID(pidFile, os.Getpid()); err != nil {
		return err
	}
	defer os.Remove(pidFile)

	bin, err := os.Executable()
	if err != nil {
		return err
	}
	bin, _ = filepath.EvalSymlinks(bin)

	const maxRestarts = 10
	const maxBackoff = 30 * time.Second
	const stableThreshold = 60 * time.Second

	consecutiveCrashes := 0
	backoff := time.Second

	for {
		startTime := time.Now()

		fmt.Fprintf(os.Stderr, "[daemon] starting gateway (port %d) at %s\n", port, startTime.Format(time.RFC3339))

		cmd := exec.Command(bin, "gateway", "--port", strconv.Itoa(port))
		// 继承 stdout/stderr（已重定向到日志文件）
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		// 打开日志文件以附加上下文
		lf, _ := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if lf != nil {
			cmd.Stdout = lf
			cmd.Stderr = lf
		}

		err := cmd.Run()

		if lf != nil {
			lf.Close()
		}

		elapsed := time.Since(startTime)

		if err == nil {
			// 正常退出
			fmt.Fprintf(os.Stderr, "[daemon] gateway exited cleanly\n")
			return nil
		}

		// 检查是否收到停止信号
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if isCleanShutdown(exitErr) {
				fmt.Fprintln(os.Stderr, "[daemon] gateway received shutdown signal, stopping")
				return nil
			}
		}

		// 网关崩溃
		if elapsed >= stableThreshold {
			// 已稳定运行足够长时间，重置退避
			consecutiveCrashes = 0
			backoff = time.Second
		}

		consecutiveCrashes++
		if consecutiveCrashes >= maxRestarts {
			return fmt.Errorf("gateway crashed %d consecutive times, giving up", maxRestarts)
		}

		fmt.Fprintf(os.Stderr, "[daemon] gateway crashed after %s (attempt %d/%d), restarting in %s\n",
			elapsed.Round(time.Second), consecutiveCrashes, maxRestarts, backoff)

		time.Sleep(backoff)

		// 指数退避：1s, 2s, 4s, 8s, 16s, 30s, 30s...
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// Stop 向守护进程发送 SIGTERM，最多等待 5 秒，然后发送 SIGKILL。
func Stop() error {
	pidFile, _, _, err := Paths()
	if err != nil {
		return err
	}

	pid, err := readPID(pidFile)
	if err != nil {
		return fmt.Errorf("daemon not running (no PID file)")
	}

	if !isProcessAlive(pid) {
		os.Remove(pidFile)
		return fmt.Errorf("daemon not running (stale PID file, cleaned up)")
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}

	// 发送 SIGTERM
	if err := signalProcess(proc, "TERM"); err != nil {
		return fmt.Errorf("signal process %d: %w", pid, err)
	}

	// 最多等待 5 秒以正常关闭
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			os.Remove(pidFile)
			fmt.Printf("Daemon stopped (PID %d)\n", pid)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 强制终止
	if err := signalProcess(proc, "KILL"); err != nil {
		// 可能已经终止
		if !isProcessAlive(pid) {
			os.Remove(pidFile)
			fmt.Printf("Daemon stopped (PID %d)\n", pid)
			return nil
		}
		return fmt.Errorf("kill process %d: %w", pid, err)
	}

	os.Remove(pidFile)
	fmt.Printf("Daemon killed (PID %d)\n", pid)
	return nil
}

// WritePIDFile 写入当前进程 PID。从 runGateway 调用。
func WritePIDFile() error {
	pidFile, _, _, err := Paths()
	if err != nil {
		return err
	}
	return writePID(pidFile, os.Getpid())
}

// RemovePIDFile 移除 PID 文件。在正常关闭时调用。
func RemovePIDFile() {
	pidFile, _, _, _ := Paths()
	if pidFile != "" {
		os.Remove(pidFile)
	}
}

// SignalReload 要求运行在 pid 的网关重新加载其内存中的缓存而无需重启。在 Unix 上这是 SIGHUP；在 Windows 上
// 返回错误，调用者打印"重启它"的提示。
func SignalReload(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return signalProcess(proc, "RELOAD")
}

func writePID(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// 通过临时文件进行原子写入
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// 信号 0 检查进程是否存在而不实际发送信号
	err = signalProcess(proc, "CHECK")
	return err == nil
}
