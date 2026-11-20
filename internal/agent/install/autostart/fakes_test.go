package autostart

import (
	"context"
	"errors"
	"strings"
)

// fakeRunner records commands. Commands whose joined text starts with a
// key in fail return that error.
// When fn is set it decides every result instead.
type fakeRunner struct {
	calls []string
	fail  map[string]error
	fn    func(cmd string) error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	c := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, c)
	if f.fn != nil {
		return "", f.fn(c)
	}
	for prefix, err := range f.fail {
		if strings.HasPrefix(c, prefix) {
			return "", err
		}
	}
	return "", nil
}

type exitErr int

func (e exitErr) Error() string { return "exit status" }
func (e exitErr) ExitCode() int { return int(e) }

var errFake = errors.New("fake failure")

type fakeSpawner struct{ spawned []string }

func (f *fakeSpawner) Spawn(bin string, args ...string) error {
	f.spawned = append(f.spawned, strings.Join(append([]string{bin}, args...), " "))
	return nil
}

type fakeRunKey struct{ values map[string]string }

func (k *fakeRunKey) Get(name string) (string, bool, error) {
	v, ok := k.values[name]
	return v, ok, nil
}

func (k *fakeRunKey) Set(name, value string) error {
	k.values[name] = value
	return nil
}

func (k *fakeRunKey) Delete(name string) error {
	delete(k.values, name)
	return nil
}
