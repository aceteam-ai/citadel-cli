// Package composefake is an in-memory container engine for tests of
// compose.ReplaceRunningService and its callers. It models containers (ID,
// name, state, compose project and service labels), images and the parts of
// `compose up` convergence those callers rely on, and can fail any step on
// demand. It never executes a process.
package composefake

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Container is one fake container.
type Container struct {
	ID, Name, State  string
	Project, Service string
	// Config is the hash of the compose file the container was created from.
	Config string
}

// Engine is the fake. Set the exported fields, then pass Exec as a
// compose.Exec.
type Engine struct {
	mu sync.Mutex
	// Project is the compose project `up` creates containers in.
	Project string
	// Service and ContainerName describe the single compose service.
	Service, ContainerName string
	Image                  string
	Build                  bool
	Images                 map[string]bool
	containers             []*Container
	// Fail maps a step to how many more times it fails: "config", "pull",
	// "build", "stop", "start", "rename", "rm", "up" (fails before any change),
	// "up-start" (new container created, old removed, start fails) and
	// "exit-after-start" (up succeeds, the new container exits at once).
	Fail   map[string]int
	Calls  []string
	nextID int
}

// New returns an engine for one service in the default "services" project.
func New(service, containerName, image string) *Engine {
	return &Engine{Project: "services", Service: service, ContainerName: containerName, Image: image, Images: map[string]bool{image: true}, Fail: map[string]int{}}
}

// Add places a container and returns it.
func (e *Engine) Add(c Container) *Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c.ID == "" {
		c.ID = e.newIDLocked()
	}
	cp := c
	e.containers = append(e.containers, &cp)
	return &cp
}

// Containers returns a snapshot of every container.
func (e *Engine) Containers() []Container {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Container, 0, len(e.containers))
	for _, c := range e.containers {
		out = append(out, *c)
	}
	return out
}

// Named returns the container with that name, if any.
func (e *Engine) Named(name string) (Container, bool) {
	for _, c := range e.Containers() {
		if c.Name == name {
			return c, true
		}
	}
	return Container{}, false
}

// Count reports how many recorded calls contain all the given words.
func (e *Engine) Count(words ...string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, call := range e.Calls {
		fields := " " + call + " "
		all := true
		for _, w := range words {
			if !strings.Contains(fields, " "+w+" ") {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// FileHash hashes a compose file the way the fake records container config.
func FileHash(path string) string {
	data, _ := os.ReadFile(path)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (e *Engine) newIDLocked() string {
	e.nextID++
	return fmt.Sprintf("fake%04d", e.nextID)
}

func (e *Engine) failLocked(step string) bool {
	if e.Fail[step] > 0 {
		e.Fail[step]--
		return true
	}
	return false
}

func (e *Engine) findLocked(ref string) *Container {
	for _, c := range e.containers {
		if c.ID == ref || c.Name == ref {
			return c
		}
	}
	return nil
}

func (e *Engine) removeLocked(id string) {
	for i, c := range e.containers {
		if c.ID == id {
			e.containers = append(e.containers[:i], e.containers[i+1:]...)
			return
		}
	}
}

// Exec implements compose.Exec.
func (e *Engine) Exec(cmd *exec.Cmd) ([]byte, []byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	args := append([]string(nil), cmd.Args...)
	e.Calls = append(e.Calls, strings.Join(args, " "))
	if len(args) == 0 {
		return nil, nil, errors.New("empty command")
	}
	bin := args[0]
	rest := args[1:]
	if strings.HasSuffix(bin, "-compose") {
		return e.composeLocked(rest)
	}
	if len(rest) > 0 && rest[0] == "compose" {
		return e.composeLocked(rest[1:])
	}
	return e.engineLocked(rest)
}

func errOut(format string, args ...any) ([]byte, []byte, error) {
	msg := fmt.Sprintf(format, args...)
	return nil, []byte(msg), errors.New("exit status 1")
}

func (e *Engine) engineLocked(args []string) ([]byte, []byte, error) {
	if len(args) == 0 {
		return errOut("no engine verb")
	}
	if args[0] == "container" && len(args) > 1 {
		args = args[1:]
	} else if args[0] == "inspect" {
		// A bare inspect would also match images, networks and volumes.
		return errOut("fake engine: use container inspect")
	}
	switch args[0] {
	case "inspect":
		c := e.findLocked(args[len(args)-1])
		if c == nil {
			return errOut("no such container")
		}
		return []byte(fmt.Sprintf("%s|%s|%s|%s\n", c.ID, c.State, c.Project, c.Service)), nil, nil
	case "image":
		if len(args) >= 3 && args[1] == "inspect" && e.Images[args[2]] {
			return []byte("[]"), nil, nil
		}
		return errOut("no such image")
	case "stop", "start":
		if e.failLocked(args[0]) {
			return errOut("%s failed", args[0])
		}
		c := e.findLocked(args[len(args)-1])
		if c == nil {
			return errOut("no such container")
		}
		if args[0] == "stop" {
			c.State = "exited"
		} else {
			c.State = "running"
		}
		return nil, nil, nil
	case "rename":
		if e.failLocked("rename") {
			return errOut("rename failed")
		}
		c := e.findLocked(args[1])
		if c == nil {
			return errOut("no such container")
		}
		if other := e.findLocked(args[2]); other != nil {
			return errOut("name %s in use", args[2])
		}
		c.Name = args[2]
		return nil, nil, nil
	case "rm":
		if e.failLocked("rm") {
			return errOut("rm failed")
		}
		c := e.findLocked(args[len(args)-1])
		if c == nil {
			return errOut("no such container")
		}
		e.removeLocked(c.ID)
		return nil, nil, nil
	}
	return nil, nil, nil
}

func composeFile(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-f" {
			return args[i+1]
		}
	}
	return ""
}

func (e *Engine) composeLocked(args []string) ([]byte, []byte, error) {
	verbAt := -1
	for i, a := range args {
		switch a {
		case "up", "config", "pull", "build":
			verbAt = i
		}
		if verbAt >= 0 {
			break
		}
	}
	if verbAt < 0 {
		return nil, nil, nil
	}
	verb := args[verbAt]
	switch verb {
	case "config":
		if e.failLocked("config") {
			return errOut("invalid interpolation")
		}
		out := fmt.Sprintf("services:\n  %s:\n    image: %s\n", e.Service, e.Image)
		if e.Build {
			out += "    build:\n      context: .\n"
		}
		return []byte(out), nil, nil
	case "pull", "build":
		if e.failLocked(verb) {
			return errOut("%s failed", verb)
		}
		e.Images[e.Image] = true
		return nil, nil, nil
	}
	force := false
	for _, a := range args[verbAt+1:] {
		if a == "--force-recreate" {
			force = true
		}
	}
	if e.failLocked("up") {
		return errOut("compose up failed")
	}
	desired := FileHash(composeFile(args))
	var mine *Container
	for _, c := range e.containers {
		if c.Project == e.Project && c.Service == e.Service {
			mine = c
		}
	}
	if mine == nil {
		if taken := e.findLocked(e.ContainerName); taken != nil {
			return errOut("Conflict. The container name %q is already in use", e.ContainerName)
		}
	}
	if mine != nil && mine.Config == desired && !force {
		mine.State = "running"
		return nil, nil, nil
	}
	created := &Container{ID: e.newIDLocked(), Name: "tmp_" + e.ContainerName, State: "created", Project: e.Project, Service: e.Service, Config: desired}
	e.containers = append(e.containers, created)
	if mine != nil {
		e.removeLocked(mine.ID)
	}
	created.Name = e.ContainerName
	if e.failLocked("up-start") {
		created.State = "exited"
		return errOut("container failed to start")
	}
	created.State = "running"
	if e.failLocked("exit-after-start") {
		created.State = "exited"
	}
	return nil, nil, nil
}
