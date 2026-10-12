// Package journal hands an event of artefact to the journal command of the
// configuration, in the background: its failure never reaches the caller.
package journal

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Entry is one event.
type Entry struct {
	ID     string // artefact id, or cli
	Type   string // unsigned
	Sphere string
	Text   string
	At     time.Time
	By     string
}

// Argv fills {id}, {type}, {by}, {at} and {sphere} in the command.
func Argv(command []string, e Entry) []string {
	r := strings.NewReplacer("{id}", e.ID, "{type}", e.Type, "{by}", e.By, "{at}", e.At.Format(time.RFC3339), "{sphere}", e.Sphere)
	out := make([]string, len(command))
	for i, a := range command {
		out[i] = r.Replace(a)
	}
	return out
}

// Start launches the command detached, the text on its stdin, and returns
// without waiting. An empty command does nothing.
func Start(command []string, e Entry) error {
	if len(command) == 0 {
		return nil
	}
	f, err := os.CreateTemp("", "artefact-journal-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // the child keeps its open descriptor
	defer f.Close()
	if _, err := f.WriteString(e.Text); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	argv := Argv(command, e)
	c := exec.Command(argv[0], argv[1:]...)
	c.Stdin = f
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return fmt.Errorf("journal %s: %w", argv[0], err)
	}
	go c.Wait()
	return c.Process.Release()
}
