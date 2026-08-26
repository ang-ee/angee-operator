package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ang-ee/angee-operator/internal/service"
	"github.com/spf13/cobra"
)

// runDevForeground runs `angee dev` in the foreground. When stdin is a real
// terminal it layers an interactive control loop over the streamed logs:
// pressing any key opens a menu to restart or quit an individual service or the
// whole stack. When stdin is not a terminal (pipes, CI, tests) it falls back to
// the plain streaming behaviour with no behavioural change.
func runDevForeground(cmd *cobra.Command, platform service.API, build bool, stdout io.Writer) error {
	ctx := cmd.Context()
	stderr := cmd.ErrOrStderr()

	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !isTerminal(int(in.Fd())) {
		return platform.StackDevForeground(ctx, build, stdout, stderr)
	}

	// The process-compose backend prompts on os.Stdin to install itself on
	// first use (see proccompose.confirmInstall / canPrompt). Taking the
	// terminal into cbreak mode and reading keys concurrently would steal that
	// prompt's input and hang startup. If the stack runs local processes and
	// process-compose isn't installed yet, stream without the interactive menu
	// for this run so the backend can run its install prompt in normal cooked
	// mode; the menu is available on the next run once the binary is in place.
	// (When process-compose is present — the bundled default — or the stack has
	// no local processes, there is no prompt and no contention.)
	if devStackHasLocalProcesses(ctx, platform) && !processComposeInstalled(ctx) {
		return platform.StackDevForeground(ctx, build, stdout, stderr)
	}

	// Derive a cancellable context so quitting the whole stack can cancel it,
	// which interrupts both runtime backends into a graceful teardown exactly
	// like Ctrl-C. The dev session runs in the background so this goroutine can
	// own the keyboard.
	devCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	devErr := make(chan error, 1)
	go func() {
		err := platform.StackDevForeground(devCtx, build, stdout, stderr)
		devErr <- err
		cancel() // wake the control loop if a backend exits on its own
	}()

	ctl := &devController{
		platform:    platform,
		in:          in,
		out:         stdout,
		cancelStack: cancel,
		rootCtx:     ctx,
	}
	ctl.run(devCtx)

	return <-devErr
}

// devStackHasLocalProcesses reports whether any stack service runs on the local
// (process-compose) runtime. Errors resolve to false: the interactive menu is
// the common path, and StackDevForeground surfaces a genuinely broken stack.
func devStackHasLocalProcesses(ctx context.Context, platform service.API) bool {
	resp, err := platform.StackStatus(ctx)
	if err != nil {
		return false
	}
	for _, s := range resp.Services {
		if s.Runtime == "local" {
			return true
		}
	}
	return false
}

// processComposeInstalled reports whether the process-compose binary is
// resolvable the same way the runtime backend resolves it: on PATH, or in the
// Go bin directory ($(go env GOPATH)/bin). Mirroring proccompose.Backend keeps
// the interactive fallback in step with the backend — it only bypasses the menu
// when the backend would actually prompt to install process-compose.
func processComposeInstalled(ctx context.Context) bool {
	if _, err := exec.LookPath("process-compose"); err == nil {
		return true
	}
	out, err := exec.CommandContext(ctx, "go", "env", "GOPATH").Output()
	if err != nil {
		return false
	}
	gopath := strings.TrimSpace(string(out))
	if gopath == "" {
		return false
	}
	_, err = os.Stat(filepath.Join(gopath, "bin", "process-compose"))
	return err == nil
}

// devController drives the interactive control menu over a running dev session.
type devController struct {
	platform    service.API
	in          *os.File
	out         io.Writer
	cancelStack context.CancelFunc
	rootCtx     context.Context
	keys        chan byte
}

// run puts the terminal in cbreak mode and processes keypresses until the dev
// session ends (context cancelled) or the user quits the whole stack.
func (c *devController) run(ctx context.Context) {
	state, err := enterCbreak(int(c.in.Fd()))
	if err != nil {
		// The terminal can't be controlled; the stack still streams in the
		// background, so just wait for it to finish.
		<-ctx.Done()
		return
	}
	defer func() {
		if err := state.restore(); err != nil {
			c.line("  warning: could not restore terminal mode: " + err.Error())
		}
	}()

	c.keys = make(chan byte, 16)
	readerDone := make(chan struct{})
	go func() {
		c.readLoop(ctx)
		close(readerDone)
	}()
	// Join the reader before restoring the terminal (deferred later, so it runs
	// first): the poll-based readLoop exits within one poll interval of ctx
	// cancellation, so no key read outlives the session or races the restore.
	defer func() { <-readerDone }()

	c.line("[angee dev] press R to restart or Q to quit a service (any other key for the menu)")

	for {
		select {
		case <-ctx.Done():
			return
		case b, ok := <-c.keys:
			if !ok {
				<-ctx.Done()
				return
			}
			// Ignore stray newlines so an accidental Enter (or the Enter after
			// an "A") doesn't open anything on its own.
			if b == '\r' || b == '\n' {
				continue
			}
			if quitAll := c.handleKey(ctx, b); quitAll {
				return
			}
		}
	}
}

// handleKey dispatches the first keypress. R and Q are direct hotkeys straight
// to the restart or quit picker — no second press — while any other key shows
// the menu hint and waits for a choice.
func (c *devController) handleKey(ctx context.Context, b byte) bool {
	switch b {
	case 'r', 'R':
		return c.pick(ctx, actionRestart)
	case 'q', 'Q':
		return c.pick(ctx, actionQuit)
	default:
		return c.menu(ctx)
	}
}

// readLoop feeds keypresses onto c.keys until stdin closes or ctx is done. It
// is the sole reader of stdin so cbreak-mode reads never contend. It polls for
// readability with a short timeout rather than blocking in Read, so it observes
// ctx cancellation within one poll interval and exits promptly on teardown
// (letting run join it before restoring the terminal).
func (c *devController) readLoop(ctx context.Context) {
	defer close(c.keys)
	fd := int(c.in.Fd())
	buf := make([]byte, 1)
	for {
		if ctx.Err() != nil {
			return
		}
		ready, err := pollReadable(fd, 200*time.Millisecond)
		if err != nil {
			return
		}
		if !ready {
			continue // timed out with no input; re-check ctx and poll again
		}
		n, err := c.in.Read(buf)
		if n > 0 {
			select {
			case c.keys <- buf[0]:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// nextKey returns the next keypress, or ok=false if the session ended. It
// checks cancellation first so a key buffered alongside a finished session does
// not win the select and drive an action during teardown.
func (c *devController) nextKey(ctx context.Context) (byte, bool) {
	if ctx.Err() != nil {
		return 0, false
	}
	select {
	case <-ctx.Done():
		return 0, false
	case b, ok := <-c.keys:
		return b, ok
	}
}

// menu shows the top-level control menu and handles one action. It returns true
// when the user chose to quit the whole stack so the caller exits.
func (c *devController) menu(ctx context.Context) bool {
	c.line("")
	c.line("  [R] restart   [Q] quit   [any other key] resume")
	b, ok := c.nextKey(ctx)
	if !ok {
		return false
	}
	switch b {
	case 'r', 'R':
		return c.pick(ctx, actionRestart)
	case 'q', 'Q':
		return c.pick(ctx, actionQuit)
	default:
		c.line("  resuming logs…")
		return false
	}
}

type devAction int

const (
	actionRestart devAction = iota
	actionQuit
)

func (a devAction) verb() string {
	if a == actionQuit {
		return "quit"
	}
	return "restart"
}

func (a devAction) title() string {
	if a == actionQuit {
		return "Quit"
	}
	return "Restart"
}

// selectAll is the picker sentinel for "the whole stack".
const selectAll = -1

// pick shows the numbered service picker for an action and applies the choice.
// It returns true only when the user quits the whole stack.
func (c *devController) pick(ctx context.Context, action devAction) bool {
	services, err := c.listServices(ctx)
	if err != nil {
		c.line("  could not list services: " + err.Error())
		return false
	}
	if len(services) == 0 {
		c.line("  no services to " + action.verb())
		return false
	}

	c.line("")
	c.line(fmt.Sprintf("  %s which service?", action.title()))
	for i, s := range services {
		c.line(fmt.Sprintf("    %2d) %-24s %-9s %s", i+1, s.Name, s.Runtime, s.stateLabel()))
	}
	c.line("     A) all services (the whole stack)")
	c.line("    Esc) cancel")
	c.print(fmt.Sprintf("  %s > ", action.verb()))

	sel, ok := c.readSelection(ctx, len(services))
	if !ok {
		c.line("")
		c.line("  cancelled")
		return false
	}
	if sel == selectAll {
		return c.applyAll(ctx, action, services)
	}
	return c.applyOne(ctx, action, services[sel])
}

// readSelection reads a service selection: a 1-based number terminated by
// Enter, "A" for all, or Esc/empty to cancel. Digits are echoed since the
// terminal is in cbreak mode (no echo). max is the number of services.
func (c *devController) readSelection(ctx context.Context, max int) (int, bool) {
	var digits []byte
	for {
		b, ok := c.nextKey(ctx)
		if !ok {
			return 0, false
		}
		switch {
		case b == 'a' || b == 'A':
			c.print("A")
			return selectAll, true
		case b == '\r' || b == '\n':
			if len(digits) == 0 {
				return 0, false
			}
			n, err := strconv.Atoi(string(digits))
			if err != nil || n < 1 || n > max {
				c.line("")
				c.line("  invalid selection")
				return 0, false
			}
			return n - 1, true
		case b == 0x1b: // Esc
			return 0, false
		case b == 0x7f || b == 0x08: // Backspace / Delete
			if len(digits) > 0 {
				digits = digits[:len(digits)-1]
				c.print("\b \b")
			}
		case b >= '0' && b <= '9':
			digits = append(digits, b)
			c.print(string(b))
		default:
			// Ignore anything else.
		}
	}
}

// applyOne applies the action to a single service. Restart starts the service
// when it is not currently running. A key buffered as the dev session ends can
// still reach here, so it bails if the session is already tearing down rather
// than acting on a stack that is going away. The action itself runs on rootCtx
// so it is not cut short by an unrelated quit-all.
func (c *devController) applyOne(ctx context.Context, action devAction, s devService) bool {
	if ctx.Err() != nil {
		return false
	}
	c.line("")
	names := []string{s.Name}
	switch action {
	case actionQuit:
		c.report("stopping "+s.Name, c.platform.ServiceStop(c.rootCtx, names))
	case actionRestart:
		if s.running() {
			c.report("restarting "+s.Name, c.platform.ServiceRestart(c.rootCtx, names))
		} else {
			c.report("starting "+s.Name, c.platform.ServiceStart(c.rootCtx, names))
		}
	}
	return false
}

// applyAll applies the action to every service. Quitting the whole stack tears
// the session down and exits; restarting starts any stopped services. Like
// applyOne it bails if the session is already tearing down.
func (c *devController) applyAll(ctx context.Context, action devAction, services []devService) bool {
	if ctx.Err() != nil {
		return false
	}
	c.line("")
	if action == actionQuit {
		c.line("  stopping the whole stack and exiting…")
		c.cancelStack()
		return true
	}

	var running, stopped []string
	for _, s := range services {
		if s.running() {
			running = append(running, s.Name)
		} else {
			stopped = append(stopped, s.Name)
		}
	}
	if len(running) > 0 {
		c.report("restarting "+strings.Join(running, ", "), c.platform.ServiceRestart(c.rootCtx, running))
	}
	if len(stopped) > 0 {
		c.report("starting "+strings.Join(stopped, ", "), c.platform.ServiceStart(c.rootCtx, stopped))
	}
	return false
}

// devService is one selectable service with its observed runtime state.
type devService struct {
	Name    string
	Runtime string
	Status  string
}

// running reports whether the service is up or on its way up, in which case a
// restart request uses ServiceRestart. Anything else — exited, stopped,
// completed, created, errored, or never observed — is treated as down and a
// restart request starts it instead. The state vocabulary spans both backends:
// docker compose reports lowercase docker states, process-compose is lowercased
// in parseList.
func (s devService) running() bool {
	switch strings.ToLower(strings.TrimSpace(s.Status)) {
	case "running", "restarting", "launching", "pending", "paused":
		return true
	default:
		return false
	}
}

func (s devService) stateLabel() string {
	st := strings.TrimSpace(s.Status)
	if st == "" {
		st = "unknown"
	}
	return "(" + st + ")"
}

// listServices returns every service in the stack, sorted by name, with its
// observed runtime state. Jobs are excluded — they are invoked, not supervised.
func (c *devController) listServices(ctx context.Context) ([]devService, error) {
	resp, err := c.platform.StackStatus(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(resp.Services))
	for name := range resp.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]devService, 0, len(names))
	for _, name := range names {
		s := resp.Services[name]
		out = append(out, devService{Name: name, Runtime: s.Runtime, Status: s.Status})
	}
	return out, nil
}

// report prints the outcome of a runtime action.
func (c *devController) report(msg string, err error) {
	if err != nil {
		c.line("  " + msg + ": " + err.Error())
		return
	}
	c.line("  " + msg + " ✓")
}

// line writes s followed by a newline. The terminal keeps ONLCR on in cbreak
// mode, so a bare "\n" renders correctly alongside the streamed child logs.
func (c *devController) line(s string) {
	_, _ = fmt.Fprint(c.out, s+"\n")
}

// print writes s with no trailing newline (used for prompts and echoed input).
func (c *devController) print(s string) {
	_, _ = fmt.Fprint(c.out, s)
}
