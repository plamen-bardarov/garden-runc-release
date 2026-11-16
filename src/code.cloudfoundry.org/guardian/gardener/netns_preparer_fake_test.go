package gardener_test

import (
	"code.cloudfoundry.org/lager/v3"
)

type fakeNetnsPreparer struct {
	pid       int
	nsPath    string
	err       error
	prepared  []string
	released  []string
	doneCalls int
}

func (f *fakeNetnsPreparer) Prepare(_ lager.Logger, handle string) (int, string, func(), error) {
	if f.err != nil {
		return 0, "", nil, f.err
	}
	f.prepared = append(f.prepared, handle)
	return f.pid, f.nsPath, func() { f.doneCalls++ }, nil
}

func (f *fakeNetnsPreparer) Release(_ lager.Logger, handle string) error {
	f.released = append(f.released, handle)
	return nil
}
