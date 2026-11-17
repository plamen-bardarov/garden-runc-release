//go:build linux

package vmreaper_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"code.cloudfoundry.org/guardian/rundmc/vmreaper"
	"code.cloudfoundry.org/lager/v3/lagertest"
)

func newReaper(t *testing.T) (*vmreaper.Reaper, string, string, *[]string) {
	cgroupRoot := t.TempDir()
	runUser := t.TempDir()
	deleted := &[]string{}
	r := vmreaper.New(cgroupRoot)
	r.RunUserDir = runUser
	r.KillTimeout = 5 * time.Second
	r.DeleteUser = func(name string) error { *deleted = append(*deleted, name); return nil }
	return r, cgroupRoot, runUser, deleted
}

func startSleep(t *testing.T) *exec.Cmd {
	cmd := exec.Command("sleep", "100")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

func alive(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	return err == nil && !containsZombie(string(stat))
}

func containsZombie(stat string) bool {
	for i := len(stat) - 1; i > 0; i-- {
		if stat[i] == ')' {
			return i+2 < len(stat) && stat[i+2] == 'Z'
		}
	}
	return false
}

func TestReapKillsProcessesLeftInTheCgroup(t *testing.T) {
	r, cgroupRoot, _, _ := newReaper(t)
	leftover := startSleep(t)

	// a sandbox cgroup with a child (kata places helpers in sub-cgroups)
	child := filepath.Join(cgroupRoot, "handle", "kata_sandbox")
	if err := os.MkdirAll(child, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "cgroup.procs"), []byte(strconv.Itoa(leftover.Process.Pid)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := r.Reap(lagertest.NewTestLogger("test"), "handle"); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if alive(leftover.Process.Pid) {
		t.Fatal("process left in the cgroup is still alive")
	}
}

func TestReapIsANoopWithoutLeftovers(t *testing.T) {
	r, _, _, deleted := newReaper(t)
	r.LookupUser = func(string) (string, string, error) { t.Fatal("no user lookup expected"); return "", "", nil }

	if err := r.Reap(lagertest.NewTestLogger("test"), "missing"); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(*deleted) != 0 {
		t.Fatalf("deleted users: %v", *deleted)
	}
}

func TestReapRemovesOnlyKataVMMUsersOfTheSandbox(t *testing.T) {
	r, _, runUser, deleted := newReaper(t)
	for _, uid := range []string{"1004", "1005"} {
		if err := os.MkdirAll(filepath.Join(runUser, uid, "run", "kata", "handle-"+uid), 0755); err != nil {
			t.Fatal(err)
		}
	}
	users := map[string][2]string{
		"1004": {"kata-7376", `"` + vmreaper.KataVMMUserComment + `"`},
		"1005": {"someone", "Some Person"},
	}
	r.LookupUser = func(uid string) (string, string, error) {
		u, ok := users[uid]
		if !ok {
			return "", "", errors.New("unknown")
		}
		return u[0], u[1], nil
	}

	log := lagertest.NewTestLogger("test")
	if err := r.Reap(log, "handle-1004"); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(*deleted) != 1 || (*deleted)[0] != "kata-7376" {
		t.Fatalf("expected kata-7376 to be removed, got %v", *deleted)
	}
	if _, err := os.Stat(filepath.Join(runUser, "1004")); !os.IsNotExist(err) {
		t.Fatalf("runtime dir of the vmm user not removed: %v", err)
	}

	// a non-kata user owning a matching runtime dir is left alone
	if err := r.Reap(log, "handle-1005"); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(*deleted) != 1 {
		t.Fatalf("non kata user removed: %v", *deleted)
	}
	if _, err := os.Stat(filepath.Join(runUser, "1005")); err != nil {
		t.Fatalf("runtime dir of a non kata user removed: %v", err)
	}
}
