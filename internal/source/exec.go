// Package source: Exec runs a child process and streams its stdout+stderr into
// the hub, so a host program that only logs to the console (a Spring Boot app, a
// dev server, any 12-factor process) shows up in the Dozzle UI with no files and
// no Docker. This is the "out of the box" path for host processes:
//
//	dozzle-shim -exec "java -jar app.jar" -name erservices
//
// The shim owns the child's lifecycle: it starts the process, captures BOTH
// pipes (many apps — including Spring Boot — log to stderr, so stdout-only would
// silently drop lines), tags each line stdout/stderr, and unlists the container
// when the process exits. Each -exec becomes one container in the UI.
package source

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/lyminhtanh/dozzle-shim/internal/hub"
	"github.com/lyminhtanh/dozzle-shim/internal/model"
)

// Exec runs one command and streams its output into the hub.
type Exec struct {
	// Command is the command line to run. By default it is executed THROUGH THE
	// SYSTEM SHELL (`sh -c "<Command>"` on Unix, `cmd /c "<Command>"` on
	// Windows), so quotes, pipes, globs and env vars work as the user expects:
	//   Command: `java -jar app.jar --spring.profiles.active=dev`
	//   Command: `npm run dev 2>&1`
	Command string
	// Args, when non-empty, runs the program directly WITHOUT a shell (Args[0]
	// is the program). Use this when no shell is available or shell parsing is
	// unwanted; it takes precedence over Command.
	Args []string
	// Name is the container label shown in the UI. Defaults to a name derived
	// from the command.
	Name string
	// Hub receives every parsed line.
	Hub *hub.Hub
	// Log is an optional logger; nil disables logging.
	Log func(format string, args ...any)

	id   string
	once sync.Once
}

func (e *Exec) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log(format, args...)
	}
}

// buildCmd constructs the *exec.Cmd: direct argv if Args is set, else the
// Command string run through the platform shell.
func (e *Exec) buildCmd(ctx context.Context) *exec.Cmd {
	if len(e.Args) > 0 {
		return exec.CommandContext(ctx, e.Args[0], e.Args[1:]...)
	}
	shell, flag := shellInvocation()
	return exec.CommandContext(ctx, shell, flag, e.Command)
}

// spec is a human-readable form of what will run (for ids/logs).
func (e *Exec) spec() string {
	if len(e.Args) > 0 {
		return strings.Join(e.Args, " ")
	}
	return e.Command
}

// isEmpty reports whether there is nothing to run.
func (e *Exec) isEmpty() bool {
	return len(e.Args) == 0 && strings.TrimSpace(e.Command) == ""
}

// containerID is a stable id derived from the command + name, so the same
// -exec maps to the same UI container across restarts.
func (e *Exec) containerID() string {
	e.once.Do(func() {
		seed := e.Name + "\x00" + e.spec()
		sum := sha256.Sum256([]byte(seed))
		e.id = "exec-" + hex.EncodeToString(sum[:])[:10]
	})
	return e.id
}

// displayName is the label: explicit Name, else the first word of the command
// with any path stripped (e.g. "/usr/bin/java -jar app.jar" -> "java").
func (e *Exec) displayName() string {
	if e.Name != "" {
		return e.Name
	}
	fields := strings.Fields(e.spec())
	if len(fields) == 0 {
		return "process"
	}
	prog := fields[0]
	if i := strings.LastIndexAny(prog, "/\\"); i >= 0 {
		prog = prog[i+1:]
	}
	return prog
}

// Run starts the child process and streams its stdout+stderr until the process
// exits or stop is closed. On exit the container is unlisted.
func (e *Exec) Run(stop <-chan struct{}) {
	if e.isEmpty() {
		e.logf("exec: empty command, skipping")
		return
	}
	id := e.containerID()
	name := e.displayName()

	// Register the container up front so the UI lists it immediately.
	e.Hub.EnsureDockerContainer(id, name, "process")

	// We manage termination ourselves (process-group kill) rather than letting
	// CommandContext kill only the direct child, so a shell that spawns the real
	// app (e.g. `sh -c "java -jar app.jar"`) takes the grandchild down too.
	cmd := e.buildCmd(context.Background())
	setupProcGroup(cmd) // put the child in its own group (Unix)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		e.logf("exec: %s stdout pipe: %v", name, err)
		e.Hub.RemoveContainer(id)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		e.logf("exec: %s stderr pipe: %v", name, err)
		e.Hub.RemoveContainer(id)
		return
	}

	if err := cmd.Start(); err != nil {
		e.logf("exec: %s start failed: %v", name, err)
		e.Hub.RemoveContainer(id)
		return
	}
	e.logf("exec: running %q as %s (%s)", e.spec(), name, id)

	// On shutdown, terminate the whole process group so shell grandchildren die.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			killProcGroup(cmd)
		case <-done:
		}
	}()

	// Stream both pipes concurrently; each line is tagged with its stream so the
	// UI shows stdout/stderr correctly and the level heuristic leans on it.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.scan(id, name, stdoutPipe, model.Stdout) }()
	go func() { defer wg.Done(); e.scan(id, name, stderrPipe, model.Stderr) }()
	wg.Wait()

	_ = cmd.Wait() // reap; exit status not surfaced to the UI

	// Process gone → unlist it (and drop its cache), unless we're shutting down
	// (stop closed) where ClearAll handles the cache.
	select {
	case <-stop:
	default:
		e.logf("exec: %s exited — unlisting", name)
		e.Hub.RemoveContainer(id)
	}
}

// scan reads one pipe line-by-line and publishes each line to the hub.
func (e *Exec) scan(id, name string, r io.Reader, logType int) {
	sc := bufio.NewScanner(r)
	// Allow long lines (stack traces, JSON logs) up to 1MB.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		e.Hub.Publish(model.LogLine{
			Type:    model.TypeLogSingle,
			LogType: logType,
			Content: line,
			Ts:      time.Now().UnixMilli(),
			Origin: &model.LogOrigin{
				ContainerID: id,
				Name:        name,
				Image:       "process",
			},
		})
	}
}
