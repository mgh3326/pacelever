package pacelever

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProbeHelper(t *testing.T) {
	mode := os.Getenv("PACELEVER_TEST_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "lock":
		_ = WithFileLock(os.Getenv("PACELEVER_TEST_LOCK"), false, func() error {
			_ = os.WriteFile(os.Getenv("PACELEVER_TEST_READY"), []byte("ready"), 0600)
			time.Sleep(time.Minute)
			return nil
		})
	case "hang":
		_ = os.WriteFile(os.Getenv("PACELEVER_TEST_PID"), []byte(strconv.Itoa(os.Getpid())), 0600)
		fmt.Fprint(os.Stdout, "❯ ")
		time.Sleep(time.Minute)
	case "reaper":
		pid, _ := strconv.Atoi(os.Getenv("PACELEVER_TEST_PGID"))
		_ = RunReaper(pid)
	case "owner":
		p := PTYProvider{ID: "commandcode", Binary: os.Getenv("PACELEVER_TEST_BINARY"), Timeout: 10 * time.Second, Guard: helperGuard}
		_ = p.Fetch(context.Background())
	}
	os.Exit(0)
}
func helperGuard(pgid int) (func(), error) {
	r, w, e := os.Pipe()
	if e != nil {
		return nil, e
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProbeHelper$")
	cmd.Env = append(os.Environ(), "PACELEVER_TEST_HELPER=reaper", "PACELEVER_TEST_PGID="+strconv.Itoa(pgid))
	cmd.ExtraFiles = []*os.File{r}
	if e = cmd.Start(); e != nil {
		r.Close()
		w.Close()
		return nil, e
	}
	r.Close()
	return func() { w.Close(); _ = cmd.Wait() }, nil
}
func helperBinary(t *testing.T, mode string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-cmd")
	exe := strings.ReplaceAll(os.Args[0], "'", "'\\''")
	script := "#!/bin/sh\nPACELEVER_TEST_HELPER=" + mode + " exec '" + exe + "' -test.run=^TestProbeHelper$\n"
	if e := os.WriteFile(path, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	return path
}
func waitFile(t *testing.T, path string) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if _, e := os.Stat(path); e == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper did not create %s", filepath.Base(path))
}
func TestKilledCallerReleasesLock(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "lock")
	ready := path + ".ready"
	cmd := exec.Command(os.Args[0], "-test.run=^TestProbeHelper$")
	cmd.Env = append(os.Environ(), "PACELEVER_TEST_HELPER=lock", "PACELEVER_TEST_LOCK="+path, "PACELEVER_TEST_READY="+ready)
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	waitFile(t, ready)
	if e := WithFileLock(path, true, func() error { return nil }); e != ErrBusy {
		t.Fatalf("expected busy, got %v", e)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if e := WithFileLock(path, true, func() error { return nil }); e != nil {
		t.Fatalf("killed owner held lock: %v", e)
	}
}
func TestProbeDeadlineReapsGroup(t *testing.T) {
	isolate(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("PACELEVER_TEST_PID", pidFile)
	p := PTYProvider{ID: "commandcode", Binary: helperBinary(t, "hang"), Timeout: 2 * time.Second}
	o := p.Fetch(context.Background())
	if o.Error == "" {
		t.Fatal("hanging probe succeeded")
	}
	data, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(string(data))
	if pid <= 1 {
		t.Fatal("probe did not start")
	}
	if e := syscall.Kill(pid, 0); e == nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		t.Fatal("probe survived deadline")
	}
}
func TestParentDeathWatchdog(t *testing.T) {
	isolate(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("PACELEVER_TEST_PID", pidFile)
	binary := helperBinary(t, "hang")
	owner := exec.Command(os.Args[0], "-test.run=^TestProbeHelper$")
	owner.Env = append(os.Environ(), "PACELEVER_TEST_HELPER=owner", "PACELEVER_TEST_BINARY="+binary)
	if e := owner.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = owner.Process.Kill(); _ = owner.Wait() }()
	waitFile(t, pidFile)
	data, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(string(data))
	defer syscall.Kill(-pid, syscall.SIGKILL)
	_ = owner.Process.Kill()
	_ = owner.Wait()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if strings.Contains(string(out), "Z") {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("probe survived killed parent")
}
