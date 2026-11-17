//go:build linux

// Package vmreaper removes what a VM based runtime (Kata Containers) can leave
// behind for a container: processes in the container's cgroup and the
// throwaway user of a rootless hypervisor.
//
// With sandbox_cgroup_only Kata runs its shim, hypervisor and virtiofsd in the
// container's cgroup. When a VM fails to start, Kata refuses to clean up
// ("VMM exit unconfirmed") and leaves virtiofsd (and the VMM user) behind:
// the cgroup cannot be removed and garden's destroy fails forever.
package vmreaper

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"code.cloudfoundry.org/lager/v3"
)

// KataVMMUserComment is the GECOS field Kata runtime-rs gives the users it
// creates for rootless hypervisors. Only such users are ever removed.
const KataVMMUserComment = "Kata Containers temporary hypervisor user"

type Reaper struct {
	// CgroupRoot is the parent of the containers' cgroups, e.g. /sys/fs/cgroup/garden.
	CgroupRoot string
	// RunUserDir is where rootless VMM users get their runtime dirs (/run/user).
	RunUserDir string
	// ProcDir is the proc filesystem (/proc).
	ProcDir     string
	KillTimeout time.Duration

	LookupUser func(uid string) (name, comment string, err error)
	DeleteUser func(name string) error
}

func New(cgroupRoot string) *Reaper {
	return &Reaper{
		CgroupRoot:  cgroupRoot,
		RunUserDir:  "/run/user",
		ProcDir:     "/proc",
		KillTimeout: 10 * time.Second,
		LookupUser:  lookupUser,
		DeleteUser:  deleteUser,
	}
}

// Reap kills all processes left in the cgroup of the container and in its
// rootless VMM user, and removes that user. It is a no-op when nothing is
// left behind.
func (r *Reaper) Reap(log lager.Logger, handle string) error {
	log = log.Session("vm-reap", lager.Data{"handle": handle})

	users := r.vmmUsers(log, handle)
	cgroupDir := filepath.Join(r.CgroupRoot, handle)

	pids := r.cgroupPids(cgroupDir)
	for _, u := range users {
		if u.name != "" {
			pids = append(pids, r.uidPids(u.uid)...)
		}
	}
	if len(pids) > 0 {
		log.Info("killing-leftover-processes", lager.Data{"pids": pids})
		if err := os.WriteFile(filepath.Join(cgroupDir, "cgroup.kill"), []byte("1"), 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Info("cgroup-kill-failed", lager.Data{"error": err.Error()})
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		if err := r.waitGone(pids); err != nil {
			return err
		}
	}

	var errs []error
	for _, u := range users {
		if u.name != "" {
			if err := r.DeleteUser(u.name); err != nil {
				errs = append(errs, fmt.Errorf("removing vmm user %s: %w", u.name, err))
				continue
			}
			log.Info("removed-vmm-user", lager.Data{"uid": u.uid, "name": u.name})
		}
		if err := os.RemoveAll(filepath.Join(r.RunUserDir, strconv.Itoa(u.uid))); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type vmmUser struct {
	uid  int
	name string // empty when the user no longer exists
}

// vmmUsers finds the rootless VMM user of the sandbox through its runtime
// directory /run/user/<uid>/run/kata/<sandbox-id>. Users that are not Kata
// VMM users are ignored.
func (r *Reaper) vmmUsers(log lager.Logger, handle string) []vmmUser {
	matches, _ := filepath.Glob(filepath.Join(r.RunUserDir, "*", "run", "kata", handle))
	var users []vmmUser
	for _, m := range matches {
		rel, err := filepath.Rel(r.RunUserDir, m)
		if err != nil {
			continue
		}
		uid, err := strconv.Atoi(strings.SplitN(rel, string(filepath.Separator), 2)[0])
		if err != nil || uid == 0 {
			continue
		}
		name, comment, err := r.LookupUser(strconv.Itoa(uid))
		if err != nil {
			users = append(users, vmmUser{uid: uid})
			continue
		}
		if !strings.Contains(comment, KataVMMUserComment) {
			log.Info("not-a-kata-vmm-user", lager.Data{"uid": uid, "name": name})
			continue
		}
		users = append(users, vmmUser{uid: uid, name: name})
	}
	return users
}

// cgroupPids returns the live processes of a cgroup and its descendants.
func (r *Reaper) cgroupPids(dir string) []int {
	var pids []int
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		f, err := os.Open(filepath.Join(path, "cgroup.procs"))
		if err != nil {
			return nil
		}
		defer f.Close()
		s := bufio.NewScanner(f)
		for s.Scan() {
			if pid, err := strconv.Atoi(strings.TrimSpace(s.Text())); err == nil && r.alive(pid) {
				pids = append(pids, pid)
			}
		}
		return nil
	})
	return pids
}

func (r *Reaper) uidPids(uid int) []int {
	entries, err := os.ReadDir(r.ProcDir)
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		info, err := os.Stat(filepath.Join(r.ProcDir, e.Name()))
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid && r.alive(pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

func (r *Reaper) waitGone(pids []int) error {
	deadline := time.Now().Add(r.KillTimeout)
	for {
		var left []int
		for _, pid := range pids {
			if r.alive(pid) {
				left = append(left, pid)
			}
		}
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes %v still alive after SIGKILL", left)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// alive reports whether a process exists and is not a zombie.
func (r *Reaper) alive(pid int) bool {
	stat, err := os.ReadFile(filepath.Join(r.ProcDir, strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	// state is the field after the parenthesised command name
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 || end+2 >= len(stat) {
		return false
	}
	return stat[end+2] != 'Z' && stat[end+2] != 'X'
}

func lookupUser(uid string) (string, string, error) {
	u, err := user.LookupId(uid)
	if err != nil {
		return "", "", err
	}
	return u.Username, u.Name, nil
}

// deleteUser removes the user. Kata creates VMM users without a home
// directory (useradd -M), so there is nothing else to remove.
func deleteUser(name string) error {
	out, err := exec.Command("userdel", name).CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 6 { // user does not exist
		return nil
	}
	if err != nil {
		return fmt.Errorf("userdel: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
