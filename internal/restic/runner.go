package restic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/ang-ee/angee-operator/internal/logctx"
)

// ExecRunner runs restic without a shell and wires password strings to
// anonymous inherited pipes. It never writes a password to argv, env, or disk.
type ExecRunner struct{}

const stdinCopyDrainDelay = 100 * time.Millisecond

// Run executes command and captures stderr separately from stdout. When
// command.Stdout is non-nil, stdout streams only to that writer and is not
// retained in CommandResult.
func (ExecRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	var result CommandResult
	if len(command.Passwords) > 0 && runtime.GOOS == "windows" {
		return result, adapterError(Unavailable, -1, "restic password file descriptors are unavailable on Windows", "Run restic-backed snapshots on a supported Unix host.", nil)
	}

	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	configureCommandCancel(cmd)
	cmd.Dir = command.Dir
	cmd.Env = append([]string(nil), command.Env...)
	var stdout, stderr bytes.Buffer
	if command.Stdout == nil {
		cmd.Stdout = &stdout
	} else {
		cmd.Stdout = command.Stdout
	}
	cmd.Stderr = &stderr

	var stdinPipe io.WriteCloser
	var stdinDone chan stdinCopyResult
	var closeStdinPipe func()
	if command.Stdin != nil {
		var err error
		stdinPipe, err = cmd.StdinPipe()
		if err != nil {
			return result, fmt.Errorf("open restic stdin pipe: %w", err)
		}
		stdinDone = make(chan stdinCopyResult, 1)
		var stdinPipeCloseOnce sync.Once
		closeStdinPipe = func() {
			stdinPipeCloseOnce.Do(func() { _ = stdinPipe.Close() })
		}
	}

	type passwordPipe struct {
		read  *os.File
		write *os.File
		value string
	}
	pipes := make([]passwordPipe, 0, len(command.Passwords))
	closePipes := func() {
		for _, pipe := range pipes {
			_ = pipe.read.Close()
			_ = pipe.write.Close()
		}
	}
	for _, password := range command.Passwords {
		read, write, err := os.Pipe()
		if err != nil {
			closePipes()
			if closeStdinPipe != nil {
				closeStdinPipe()
			}
			return result, fmt.Errorf("open restic password pipe: %w", err)
		}
		pipes = append(pipes, passwordPipe{read: read, write: write, value: password})
		cmd.ExtraFiles = append(cmd.ExtraFiles, read)
	}

	trace := logctx.TraceExec(ctx, command.Name, command.Args, command.Dir, slog.Any("env", logctx.EnvKeys(command.Env)))
	if err := cmd.Start(); err != nil {
		closePipes()
		if closeStdinPipe != nil {
			closeStdinPipe()
		}
		trace(nil, errors.New(redactSecrets(err.Error(), command.Secrets)))
		return result, err
	}
	for _, pipe := range pipes {
		_ = pipe.read.Close()
	}
	if stdinPipe != nil {
		go func() {
			copyResult := copyCommandStdin(cmd.Process, stdinPipe, command.Stdin)
			closeStdinPipe()
			stdinDone <- copyResult
		}()
	}
	var closeCallerInput func()
	if closer, ok := command.Stdin.(io.Closer); ok {
		var callerCloseOnce sync.Once
		closeCallerInput = func() {
			callerCloseOnce.Do(func() { _ = closer.Close() })
		}
	} else {
		closeCallerInput = func() {}
	}
	closeInput := func() {
		if closeStdinPipe != nil {
			closeStdinPipe()
		}
		closeCallerInput()
	}
	stopInputCancel := func() bool { return false }
	if stdinPipe != nil {
		stopInputCancel = context.AfterFunc(ctx, func() {
			// Terminate restic before closing its input. Otherwise it can observe
			// EOF and commit a partial snapshot before CommandContext cancellation
			// reaches the process.
			_ = terminateProcess(cmd.Process)
			closeInput()
		})
	}

	writeErrors := make(chan error, len(pipes))
	var writers sync.WaitGroup
	for _, pipe := range pipes {
		writers.Add(1)
		go func(pipe passwordPipe) {
			defer writers.Done()
			_, writeErr := io.WriteString(pipe.write, pipe.value+"\n")
			closeErr := pipe.write.Close()
			writeErrors <- errors.Join(writeErr, closeErr)
		}(pipe)
	}
	runErr := cmd.Wait()
	stopInputCancel()
	if stdinPipe != nil {
		closeInput()
		// A clean exit after cancellation is not proof restic saw EOF, so only
		// an uncancelled clean exit waits for the copier without a bound.
		if runErr == nil && ctx.Err() == nil {
			copyResult := <-stdinDone
			result.StdinBytes = copyResult.bytes
			result.StdinError = copyResult.err
			result.StdinAborted = copyResult.aborted
		} else {
			select {
			case copyResult := <-stdinDone:
				result.StdinBytes = copyResult.bytes
				result.StdinError = copyResult.err
				result.StdinAborted = copyResult.aborted
			case <-time.After(stdinCopyDrainDelay):
			}
		}
	}
	writers.Wait()
	close(writeErrors)
	for writeErr := range writeErrors {
		if writeErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("write restic password pipe: %w", writeErr))
		}
	}
	result.Stdout = stdout.Bytes()
	result.Stderr = stderr.Bytes()
	traceOutput := redactSecretBytes(logctx.CombinedOutput(result.Stdout, result.Stderr), command.Secrets)
	traceErr := runErr
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		traceErr = errors.New(redactSecrets(runErr.Error(), command.Secrets))
	}
	trace(traceOutput, traceErr)
	return result, runErr
}

type stdinCopyResult struct {
	bytes   int64
	err     error
	aborted bool
}

func copyCommandStdin(process *os.Process, dst io.Writer, src io.Reader) stdinCopyResult {
	var result stdinCopyResult
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := src.Read(buffer)
		result.bytes += int64(n)
		if n > 0 {
			written, writeErr := dst.Write(buffer[:n])
			if writeErr != nil || written != n {
				return result
			}
		}
		if readErr == nil {
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			result.err = readErr
			result.aborted = terminateProcess(process) == nil
		}
		return result
	}
}
