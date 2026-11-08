package compose

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ang-ee/angee-operator/internal/runtime"
)

const streamIntegrationPayloadSize = 16 << 20

func TestStreamJobsCarryBinaryDataAndCleanUpFailures(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("streaming container jobs require inherited file descriptors and /dev/fd/3")
	}
	requireDockerCompose(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.Mkdir(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, streamIntegrationPayloadSize)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "payload"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "marker"), []byte("relative bind resolved"), 0600); err != nil {
		t.Fatal(err)
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	project := "angee-p1-" + hex.EncodeToString(suffix)
	volume := ShortVolume("./data:/data:ro")
	document := File{Name: project, Services: map[string]Service{
		"save": {
			Image: "alpine:3.24", Command: []string{"sh", "-c", "cat /data/payload; echo save-diagnostic >&2"}, Volumes: []ServiceVolume{volume},
		},
		"restore": {
			Image: "alpine:3.24", Command: []string{"sh", "-c", "test -f /data/marker; cat > /tmp/restored; sha256sum /tmp/restored >&2"}, Volumes: []ServiceVolume{volume},
		},
		"fail": {
			Image: "alpine:3.24", Command: []string{"sh", "-c", "head -c 1048576 /data/payload; exit 7"}, Volumes: []ServiceVolume{volume},
		},
		"endless": {
			Image: "alpine:3.24", Command: []string{"sh", "-c", "trap '' INT TERM; while :; do cat /data/payload; done"}, Volumes: []ServiceVolume{volume},
		},
		"create-window": {
			Image: "alpine:3.24", Command: []string{"sh", "-c", "echo create-window-started; sleep 30"},
		},
	}}
	configuration, err := Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	invalidConfiguration := []byte(fmt.Sprintf("name: %s\nservices:\n  precreate-fail:\n    image: alpine:3.24\n    definitely_not_a_compose_field: true\n", project))
	backend := NewBackend()
	target := runtime.Target{Root: root}
	t.Cleanup(func() { removeProjectResources(project) })

	t.Run("stdout clean and relative project directory", func(t *testing.T) {
		var artifact bytes.Buffer
		diagnostics, err := backend.RunJobStreams(t.Context(), target, runtime.JobSpec{Name: "save", Configuration: configuration, Transient: true}, runtime.JobStreams{
			Role: runtime.StreamRoleSave, Stdout: &artifact,
		})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := sha256.Sum256(artifact.Bytes()), sha256.Sum256(payload); got != want {
			t.Fatalf("artifact sha256 = %x, want %x", got, want)
		}
		if !bytes.Contains(diagnostics, []byte("save-diagnostic")) || len(diagnostics) >= 64<<10 {
			t.Fatalf("diagnostics = %d bytes and %q, want a small native-output tail", len(diagnostics), diagnostics)
		}

		diagnostics, err = backend.RunJobStreams(t.Context(), target, runtime.JobSpec{Name: "restore", Configuration: configuration, Transient: true}, runtime.JobStreams{
			Role: runtime.StreamRoleRestore, Stdin: bytes.NewReader(payload),
		})
		if err != nil {
			t.Fatal(err)
		}
		wantHash := fmt.Sprintf("%x", sha256.Sum256(payload))
		if !strings.Contains(string(diagnostics), wantHash) {
			t.Fatalf("restore diagnostics = %q, want sha256 %s", diagnostics, wantHash)
		}
	})

	t.Run("producer failure", func(t *testing.T) {
		var artifact bytes.Buffer
		_, err := backend.RunJobStreams(t.Context(), target, runtime.JobSpec{Name: "fail", Configuration: configuration, Transient: true}, runtime.JobStreams{
			Role: runtime.StreamRoleSave, Stdout: &artifact,
		})
		if err == nil {
			t.Fatal("producer failure returned success")
		}
		if artifact.Len() != 1<<20 {
			t.Fatalf("partial artifact = %d bytes, want 1 MiB", artifact.Len())
		}
		assertNoProjectContainer(t, project, "fail")
	})

	t.Run("failure before create", func(t *testing.T) {
		started := time.Now()
		_, err := backend.RunJobStreams(t.Context(), target, runtime.JobSpec{Name: "precreate-fail", Configuration: invalidConfiguration, Transient: true}, runtime.JobStreams{
			Role: runtime.StreamRoleCheck, Stdout: io.Discard,
		})
		elapsed := time.Since(started)
		t.Logf("failure before create returned in %s", elapsed)
		if err == nil {
			t.Fatal("invalid compose configuration returned success")
		}
		if strings.Contains(err.Error(), "may remain") {
			t.Fatalf("failure before create reported a surviving container: %v", err)
		}
		if elapsed > 5*time.Second {
			t.Fatalf("failure before create returned after %s, want within a few seconds", elapsed)
		}
		assertNoProjectContainer(t, project, "precreate-fail")
	})

	t.Run("consumer death", func(t *testing.T) {
		consumerErr := errors.New("consumer closed")
		writer := &errorAfterWriter{remaining: 1 << 20, err: consumerErr}
		_, err := backend.RunJobStreams(t.Context(), target, runtime.JobSpec{Name: "endless", Configuration: configuration, Transient: true}, runtime.JobStreams{
			Role: runtime.StreamRoleSave, Stdout: writer,
		})
		if !errors.Is(err, consumerErr) {
			t.Fatalf("consumer failure = %v, want %v", err, consumerErr)
		}
		assertNoProjectContainer(t, project, "endless")
	})

	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		started := make(chan struct{})
		writer := &notifyingWriter{started: started}
		finished := make(chan error, 1)
		go func() {
			_, err := backend.RunJobStreams(ctx, target, runtime.JobSpec{Name: "endless", Configuration: configuration, Transient: true}, runtime.JobStreams{
				Role: runtime.StreamRoleSave, Stdout: writer,
			})
			finished <- err
		}()
		select {
		case <-started:
			cancel()
		case <-time.After(30 * time.Second):
			cancel()
			t.Fatal("container did not start producing output")
		}
		select {
		case err := <-finished:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error = %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("canceled stream job did not return")
		}
		assertNoProjectContainer(t, project, "endless")
	})

	t.Run("cancel across container creation", func(t *testing.T) {
		creates := startDockerCreateCollector(t, project+"-create-window-run-")
		for i := range 20 {
			func() {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var started chan struct{}
				stdout := io.Writer(io.Discard)
				if i == 0 {
					// A pre-canceled endpoint cannot reach Docker, so it proves the
					// sweep includes the pre-create side without scheduler timing.
					cancel()
				}
				if i == 19 {
					// The final endpoint cancels only after container stdout arrives,
					// which proves Docker emitted create on the post-create side.
					started = make(chan struct{})
					stdout = &notifyingWriter{started: started}
				}
				finished := make(chan error, 1)
				go func() {
					_, err := backend.RunJobStreams(ctx, target, runtime.JobSpec{Name: "create-window", Configuration: configuration, Transient: true}, runtime.JobStreams{
						Role: runtime.StreamRoleCheck, Stdout: stdout,
					})
					finished <- err
				}()
				switch i {
				case 0:
				case 19:
					select {
					case <-started:
						cancel()
					case <-time.After(30 * time.Second):
						cancel()
						t.Fatal("post-create endpoint did not produce container output")
					}
				default:
					delay := 50*time.Millisecond + time.Duration(i-1)*(150*time.Millisecond/17)
					<-time.After(delay)
					cancel()
				}
				select {
				case err := <-finished:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancel-window run %d: error = %v", i, err)
					}
				case <-time.After(30 * time.Second):
					t.Fatalf("cancel-window run %d did not return", i)
				}
				assertNoProjectContainer(t, project, "create-window")
			}()
		}
		created := creates.stop(t)
		if created == 0 {
			t.Fatal("no create event was observed across the cancellation sweep")
		}
		t.Logf("%d of 18 timed cancellation runs observed a create event", created-1)
	})
}

type dockerCreateCollector struct {
	cancel context.CancelFunc
	cmd    *exec.Cmd
	done   chan struct{}
	prefix string

	mu       sync.Mutex
	names    map[string]struct{}
	scanErr  error
	stopOnce sync.Once
	count    int
}

func startDockerCreateCollector(t *testing.T, prefix string) *dockerCreateCollector {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	args := []string{
		"events", "--format", "{{json .}}", "--since", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		"--filter", "type=container", "--filter", "event=create",
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start Docker create-event collector: %v: %s", err, stderr.String())
	}
	collector := &dockerCreateCollector{
		cancel: cancel, cmd: cmd, done: make(chan struct{}), prefix: prefix, names: make(map[string]struct{}),
	}
	go func() {
		defer close(collector.done)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			var event struct {
				Action string `json:"Action"`
				Status string `json:"status"`
				Actor  struct {
					Attributes map[string]string `json:"Attributes"`
				} `json:"Actor"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				continue
			}
			action := event.Action
			if action == "" {
				action = event.Status
			}
			name := event.Actor.Attributes["name"]
			if strings.EqualFold(action, "create") && strings.HasPrefix(name, prefix) {
				collector.mu.Lock()
				collector.names[name] = struct{}{}
				collector.mu.Unlock()
			}
		}
		collector.mu.Lock()
		collector.scanErr = scanner.Err()
		collector.mu.Unlock()
	}()
	t.Cleanup(func() { collector.stop(t) })
	return collector
}

func (c *dockerCreateCollector) stop(t *testing.T) int {
	t.Helper()
	c.stopOnce.Do(func() {
		c.cancel()
		_ = c.cmd.Wait()
		<-c.done
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.scanErr != nil {
			t.Fatalf("read Docker create events: %v", c.scanErr)
		}
		c.count = len(c.names)
	})
	return c.count
}

type errorAfterWriter struct {
	remaining int
	err       error
}

func (w *errorAfterWriter) Write(data []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, w.err
	}
	n := min(len(data), w.remaining)
	w.remaining -= n
	if n != len(data) {
		return n, w.err
	}
	return n, nil
}

type notifyingWriter struct {
	once    sync.Once
	started chan struct{}
}

func (w *notifyingWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	return len(data), nil
}

func requireDockerCompose(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("docker compose integration")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker unavailable")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("docker compose unavailable: %v: %s", err, output)
	}
	if output, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon unavailable: %v: %s", err, output)
	}
}

func assertNoProjectContainer(t *testing.T, project, service string) {
	t.Helper()
	filter := project + "-" + service + "-run-"
	out, err := exec.CommandContext(t.Context(), "docker", "ps", "-aq", "--filter", "name="+filter).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("container from %s still exists: %s", filter, out)
	}
}

func removeProjectResources(project string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "name="+project+"-").Output()
	if err == nil {
		ids := strings.Fields(string(out))
		if len(ids) != 0 {
			_ = exec.CommandContext(ctx, "docker", append([]string{"rm", "-f", "-v"}, ids...)...).Run()
		}
	}
	_ = exec.CommandContext(ctx, "docker", "network", "rm", project+"_default").Run()
}
