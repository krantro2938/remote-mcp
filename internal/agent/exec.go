package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/krantro2938/remote-mcp/internal/proto"
)

const outputCap = 64 << 10 // keep the last 64 KiB of output

// tailBuffer keeps the last outputCap bytes written to it.
type tailBuffer struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > outputCap {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-outputCap:]...)
		t.truncated = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.ToValidUTF8(string(t.buf), "?"), t.truncated
}

type job struct {
	id      string
	cmd     *exec.Cmd
	out     *tailBuffer
	started time.Time
	done    chan struct{}
	exit    int
	timeout bool
	ended   time.Time
}

type jobTable struct {
	mu   sync.Mutex
	jobs map[string]*job
}

func newJobTable() *jobTable { return &jobTable{jobs: map[string]*job{}} }

func buildCommand(p proto.RunParams) (*exec.Cmd, error) {
	shell := strings.ToLower(p.Shell)
	if shell == "" {
		if runtime.GOOS == "windows" {
			shell = "powershell"
		} else {
			shell = "sh"
		}
	}
	var cmd *exec.Cmd
	switch shell {
	case "powershell", "pwsh":
		exe := "powershell.exe"
		if shell == "pwsh" {
			exe = "pwsh.exe"
		}
		// Force UTF-8 so Cyrillic paths and messages survive.
		script := "[Console]::OutputEncoding=[Text.Encoding]::UTF8; $OutputEncoding=[Text.Encoding]::UTF8; $ProgressPreference='SilentlyContinue'; " + p.Command
		cmd = exec.Command(exe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	case "cmd":
		cmd = exec.Command("cmd.exe", "/D", "/S", "/C", "chcp 65001>nul & "+p.Command)
	case "sh", "bash":
		cmd = exec.Command(shell, "-c", p.Command)
	default:
		return nil, fmt.Errorf("unknown shell %q (use powershell, pwsh, cmd or sh)", p.Shell)
	}
	if p.Cwd != "" {
		cmd.Dir = p.Cwd
	}
	hideWindow(cmd)
	return cmd, nil
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (t *jobTable) run(ctx context.Context, p proto.RunParams) (*proto.RunResult, error) {
	if strings.TrimSpace(p.Command) == "" {
		return nil, errors.New("command is empty")
	}
	cmd, err := buildCommand(p)
	if err != nil {
		return nil, err
	}
	out := &tailBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	j := &job{id: newID(), cmd: cmd, out: out, started: time.Now(), done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		j.exit = exitCode(err)
		j.ended = time.Now()
		close(j.done)
	}()

	timeout := time.Duration(p.TimeoutSec) * time.Second
	if p.Background {
		t.mu.Lock()
		t.jobs[j.id] = j
		t.gcLocked()
		t.mu.Unlock()
		if timeout > 0 {
			go func() {
				select {
				case <-j.done:
				case <-time.After(timeout):
					j.timeout = true
					killTree(cmd)
				}
			}()
		}
		// Give fast commands a moment so trivial errors show up immediately.
		select {
		case <-j.done:
		case <-time.After(500 * time.Millisecond):
		}
		return j.result(), nil
	}

	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	select {
	case <-j.done:
	case <-time.After(timeout):
		j.timeout = true
		killTree(cmd)
		<-j.done
	case <-ctx.Done():
		killTree(cmd)
		<-j.done
	}
	r := j.result()
	r.JobID = ""
	return r, nil
}

func (j *job) result() *proto.RunResult {
	r := &proto.RunResult{JobID: j.id}
	select {
	case <-j.done:
		r.ExitCode = j.exit
		r.DurationMs = j.ended.Sub(j.started).Milliseconds()
	default:
		r.Running = true
		r.DurationMs = time.Since(j.started).Milliseconds()
	}
	r.TimedOut = j.timeout
	r.Output, r.Truncated = j.out.String()
	return r
}

func (t *jobTable) status(p proto.JobParams) (*proto.RunResult, error) {
	t.mu.Lock()
	j, ok := t.jobs[p.JobID]
	t.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no job %q", p.JobID)
	}
	if p.Kill {
		killTree(j.cmd)
		select {
		case <-j.done:
		case <-time.After(5 * time.Second):
		}
	}
	return j.result(), nil
}

// gcLocked forgets finished jobs older than an hour.
func (t *jobTable) gcLocked() {
	for id, j := range t.jobs {
		select {
		case <-j.done:
			if time.Since(j.ended) > time.Hour {
				delete(t.jobs, id)
			}
		default:
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func readFile(p proto.ReadFileParams) (*proto.ReadFileResult, error) {
	st, err := os.Stat(p.Path)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		entries, err := os.ReadDir(p.Path)
		if err != nil {
			return nil, err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		var b strings.Builder
		for i, e := range entries {
			if i >= 2000 {
				fmt.Fprintf(&b, "… %d more\n", len(entries)-i)
				break
			}
			info, _ := e.Info()
			switch {
			case e.IsDir():
				fmt.Fprintf(&b, "%s/\n", e.Name())
			case info != nil:
				fmt.Fprintf(&b, "%s\t%d\t%s\n", e.Name(), info.Size(), info.ModTime().Format("2006-01-02 15:04"))
			default:
				fmt.Fprintf(&b, "%s\n", e.Name())
			}
		}
		return &proto.ReadFileResult{IsDir: true, Listing: b.String()}, nil
	}
	maxBytes := p.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 256 << 10
	}
	maxBytes = min(maxBytes, 20<<20)
	f, err := os.Open(p.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if p.Offset > 0 {
		if _, err := f.Seek(p.Offset, 0); err != nil {
			return nil, err
		}
	}
	buf := make([]byte, maxBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return &proto.ReadFileResult{
		Content:   buf[:n],
		Size:      st.Size(),
		Truncated: p.Offset+int64(n) < st.Size(),
	}, nil
}

func writeFile(p proto.WriteFileParams) (*proto.WriteFileResult, error) {
	if p.Path == "" {
		return nil, errors.New("path is empty")
	}
	if p.Mkdirs {
		if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
			return nil, err
		}
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if p.Append {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(p.Path, flags, 0o644)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(p.Content); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	st, err := os.Stat(p.Path)
	if err != nil {
		return nil, err
	}
	return &proto.WriteFileResult{Size: st.Size()}, nil
}
