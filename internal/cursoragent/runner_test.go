package cursoragent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func testRunner(t *testing.T, script string) Runner {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	file := filepath.Join(t.TempDir(), "synthetic.py")
	if err := os.WriteFile(file, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	return Runner{Config{Python: python, Worker: file, Model: "synthetic", Key: "synthetic-not-a-credential"}}
}

func TestProtocolAndIsolatedEnvironment(t *testing.T) {
	t.Setenv("YOUTRACK_HOMELAB_TOKEN", "synthetic-parent-marker")
	r := testRunner(t, `import json, os, sys
request=json.loads(sys.stdin.readline())
assert request['prompt']=='synthetic prompt'
assert 'YOUTRACK_HOMELAB_TOKEN' not in os.environ
assert os.getcwd()==os.path.realpath(os.environ['HOME'])
print(json.dumps({'type':'tool','id':1,'name':'youtrack_issue','args':{}}),flush=True)
assert json.loads(sys.stdin.readline())['result']=={'synthetic':True}
print(json.dumps({'type':'result','text':'synthetic result'}),flush=True)
`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := r.Run(ctx, "synthetic prompt", func(_ context.Context, name string, args json.RawMessage) any {
		if name != "youtrack_issue" || string(args) != "{}" {
			t.Error("tool mismatch")
		}
		return map[string]bool{"synthetic": true}
	}, func(string) {})
	if err != nil || got != "synthetic result" {
		t.Fatalf("result=%q err=%v", got, err)
	}
}

func TestPrivateDescendantsStopOnCancellationAndSuccess(t *testing.T) {
	for _, normal := range []bool{false, true} {
		marker := filepath.Join(t.TempDir(), "descendant-survived")
		child := fmt.Sprintf("import time,pathlib;time.sleep(0.5);pathlib.Path(%q).write_text('synthetic')", marker)
		script := fmt.Sprintf("import subprocess,sys\nsubprocess.Popen([sys.executable,'-c',%q],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)\n", child)
		if normal {
			script += "print('{\"type\":\"result\",\"text\":\"synthetic\"}',flush=True)\n"
		} else {
			script += "import time;time.sleep(60)\n"
		}
		r := testRunner(t, script)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := r.Run(ctx, "synthetic", nil, func(string) {})
		cancel()
		if (err == nil) != normal {
			t.Fatalf("normal=%v err=%v", normal, err)
		}
		time.Sleep(700 * time.Millisecond)
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("descendant executed after Run returned", err)
		}
	}
}

func TestInvalidAndFailedWorkersNeverSucceed(t *testing.T) {
	for _, script := range []string{
		`print('{"type":"result","text":"synthetic"}');raise SystemExit(1)`,
		`print('{"type":"result","text":"synthetic","text":"duplicate"}')`,
		`print('{"type":"progress","stage":"arbitrary payload"}')`,
		`print('{"type":"result","text":"synthetic"}');print('{"type":"error"}')`,
		`print('{"type":"result","text":"synthetic"}');print('{"type":"progress","stage":"running"}')`,
	} {
		r := testRunner(t, script)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := r.Run(ctx, "synthetic", nil, func(string) {})
		cancel()
		if err == nil || result != "" {
			t.Fatalf("unexpected success: %q", result)
		}
	}
}

func TestToolPolicyRejectsBeforeCallback(t *testing.T) {
	for _, payload := range []string{
		`{"type":"tool","id":1,"name":"shell","args":{}}`,
		`{"type":"tool","id":1,"name":"youtrack_issue","args":{"id":"HL-9"}}`,
		`{"type":"tool","id":1,"name":"youtrack_comments","args":{"skip":-1}}`,
		`{"type":"tool","id":1,"name":"youtrack_comments","args":{"skip":null}}`,
		`{"type":"tool","id":1,"name":"youtrack_article","args":{"id":"https://untrusted.invalid"}}`,
	} {
		r := testRunner(t, "print('"+payload+"',flush=True)")
		called := false
		_, err := r.Run(context.Background(), "synthetic", func(context.Context, string, json.RawMessage) any { called = true; return nil }, func(string) {})
		if err == nil || called {
			t.Fatal("unsafe tool reached callback", payload)
		}
	}
}

func TestTimeoutStopsPrivateProcess(t *testing.T) {
	r := testRunner(t, `import time;time.sleep(60)`)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, "synthetic", nil, func(string) {})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatal("timeout did not terminate worker")
	}
}

func TestMissingKeyDoesNotStartWorker(t *testing.T) {
	_, err := (Runner{}).Run(context.Background(), "synthetic", nil, func(string) {})
	if err == nil {
		t.Fatal("missing key accepted")
	}
}
