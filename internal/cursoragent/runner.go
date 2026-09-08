// Package cursoragent runs the actual Python Cursor SDK in a private process.
package cursoragent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

var ErrRun = errors.New("cursor_run_failed")

type Config struct{ Python, Worker, Model, Key string }
type Tool func(context.Context, string, json.RawMessage) any
type Executor interface {
	Run(context.Context, string, Tool, func(string)) (string, error)
}
type Runner struct{ Config Config }

func (r Runner) Run(ctx context.Context, prompt string, tool Tool, progress func(string)) (string, error) {
	if r.Config.Key == "" || ctx.Err() != nil {
		return "", ErrRun
	}
	dir, err := os.MkdirTemp("", "panel-agent-")
	if err != nil {
		return "", ErrRun
	}
	defer os.RemoveAll(dir) // only the private, generated workspace from MkdirTemp
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.Command(r.Config.Python, "-I", "-B", r.Config.Worker)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + filepath.Dir(r.Config.Python) + ":/usr/local/bin:/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "CURSOR_API_KEY=" + r.Config.Key}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return "", ErrRun
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return "", ErrRun
	}
	if cmd.Start() != nil {
		return "", ErrRun
	}
	// Kill the entire SDK/bridge process group, not merely the Python parent.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); <-ctx.Done(); _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
	waited := false
	defer func() {
		cancel()
		wg.Wait()
		if !waited {
			_ = cmd.Wait()
		}
	}()
	enc := json.NewEncoder(in)
	if enc.Encode(map[string]string{"model": r.Config.Model, "prompt": prompt}) != nil {
		return "", ErrRun
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var result string
	tools, events := 0, 0
	for scanner.Scan() {
		events++
		if events > 10000 || !strictjson.Valid(scanner.Bytes()) {
			return "", ErrRun
		}
		var event struct {
			Type, Stage, Name, Text, Code string
			ID                            int
			Args                          json.RawMessage
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			return "", ErrRun
		}
		if result != "" {
			return "", ErrRun
		}
		switch event.Type {
		case "progress":
			switch event.Stage {
			case "running", "analyzing", "reading_youtrack", "writing_answer":
				progress(event.Stage)
			default:
				return "", ErrRun
			}
		case "tool":
			tools++
			if tools > 40 || event.ID != tools || tool == nil || !validTool(event.Name, event.Args) {
				return "", ErrRun
			}
			if enc.Encode(map[string]any{"id": event.ID, "result": tool(ctx, event.Name, event.Args)}) != nil {
				return "", ErrRun
			}
		case "result":
			if result != "" || event.Text == "" || len(event.Text) > 65536 {
				return "", ErrRun
			}
			result = event.Text
		case "error":
			return "", ErrRun
		default:
			return "", ErrRun
		}
	}
	if scanner.Err() != nil || ctx.Err() != nil || result == "" {
		return "", ErrRun
	}
	// A valid terminal message alone does not excuse a failed worker process.
	err = cmd.Wait()
	waited = true
	if err != nil || ctx.Err() != nil {
		return "", ErrRun
	}
	// Clean up any SDK descendants before removing the generated workspace.
	return result, nil
}

const Timeout = 10 * time.Minute

var articleID = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-A-[1-9][0-9]*$`)

// SDK schemas are descriptions for the model, not a security boundary. Enforce
// the exact read-only protocol before entering the credential-owning callback.
// The callback must additionally enforce the run grant and configured project.
func validTool(name string, raw json.RawMessage) bool {
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil || args == nil {
		return false
	}
	switch name {
	case "youtrack_issue":
		return len(args) == 0
	case "youtrack_comments", "youtrack_articles":
		var skip int
		value, ok := args["skip"]
		return len(args) == 1 && ok && json.Unmarshal(value, &skip) == nil && string(value) != "null" && skip >= 0 && skip <= 100000
	case "youtrack_article":
		var id string
		return len(args) == 1 && json.Unmarshal(args["id"], &id) == nil && len(id) <= 64 && articleID.MatchString(id)
	default:
		return false
	}
}
