//go:build linux

package guardiancmd

import (
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

func TestSeccompNewerSyscallsAreAllowedOrENOSYS(t *testing.T) {
	seccomp, err := buildSeccomp()
	if err != nil {
		t.Fatal(err)
	}

	allowed := map[string]bool{}
	enosys := map[string]bool{}
	for _, rule := range seccomp.Syscalls {
		for _, name := range rule.Names {
			switch {
			case rule.Action == specs.ActAllow:
				allowed[name] = true
			case rule.Action == specs.ActErrno && rule.ErrnoRet != nil && *rule.ErrnoRet == uint(unix.ENOSYS):
				enosys[name] = true
			}
		}
	}

	for _, name := range []string{"faccessat", "faccessat2", "fchmodat", "fchmodat2", "openat2"} {
		if !allowed[name] {
			t.Errorf("%s must be allowed", name)
		}
	}
	for name := range enosys {
		if allowed[name] {
			t.Errorf("%s is both allowed and ENOSYS", name)
		}
	}
	// libc falls back to older syscalls only on ENOSYS, not on the default EPERM
	for _, name := range []string{"clone3", "epoll_pwait2", "futex_waitv", "cachestat", "map_shadow_stack", "mseal"} {
		if !enosys[name] {
			t.Errorf("%s must return ENOSYS", name)
		}
	}
}
