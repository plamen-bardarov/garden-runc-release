package nerd

import (
	"fmt"
	"syscall"
	"time"

	"code.cloudfoundry.org/garden"
	"code.cloudfoundry.org/guardian/gardener"
	"code.cloudfoundry.org/lager/v3"
	cgroup1stats "github.com/containerd/cgroups/v3/cgroup1/stats"
	cgroup2stats "github.com/containerd/cgroups/v3/cgroup2/stats"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/typeurl/v2"
)

// Stats returns container metrics as reported by the runtime shim through
// containerd (task.Metrics). It is used for VM based runtimes, where
// `runc events --stats` is not available and the host cgroup only contains
// the hypervisor threads rather than the container processes.
func (n *Nerd) Stats(log lager.Logger, id string) (gardener.StatsContainerMetrics, error) {
	log = log.Session("stats", lager.Data{"containerID": id})

	container, task, err := n.loadContainerAndTask(log, id)
	if err != nil {
		return gardener.StatsContainerMetrics{}, err
	}

	metric, err := task.Metrics(n.context)
	if err != nil {
		return gardener.StatsContainerMetrics{}, fmt.Errorf("task metrics: %w", err)
	}

	stats := gardener.StatsContainerMetrics{}
	if metric != nil && metric.Data != nil {
		data, err := typeurl.UnmarshalAny(metric.Data)
		if err != nil {
			return gardener.StatsContainerMetrics{}, fmt.Errorf("decode task metrics: %w", err)
		}

		switch m := data.(type) {
		case *cgroup2stats.Metrics:
			stats = statsFromCgroup2(m)
		case *cgroup1stats.Metrics:
			stats = statsFromCgroup1(m)
		default:
			log.Info("unsupported-metrics-type", lager.Data{"type": fmt.Sprintf("%T", data)})
		}
	}

	info, err := container.Info(n.context)
	if err == nil {
		stats.Age = time.Since(info.CreatedAt)
	}

	return stats, nil
}

func statsFromCgroup2(m *cgroup2stats.Metrics) gardener.StatsContainerMetrics {
	stats := gardener.StatsContainerMetrics{}

	if m.CPU != nil {
		// cgroup v2 reports microseconds, garden expects nanoseconds
		stats.CPU = garden.ContainerCPUStat{
			Usage:  m.CPU.UsageUsec * 1000,
			User:   m.CPU.UserUsec * 1000,
			System: m.CPU.SystemUsec * 1000,
		}
	}

	if m.Memory != nil {
		stats.Memory = garden.ContainerMemoryStat{
			Anon:                    m.Memory.Anon,
			File:                    m.Memory.File,
			ActiveAnon:              m.Memory.ActiveAnon,
			InactiveAnon:            m.Memory.InactiveAnon,
			ActiveFile:              m.Memory.ActiveFile,
			InactiveFile:            m.Memory.InactiveFile,
			MappedFile:              m.Memory.FileMapped,
			Unevictable:             m.Memory.Unevictable,
			Pgfault:                 m.Memory.Pgfault,
			Pgmajfault:              m.Memory.Pgmajfault,
			Swap:                    m.Memory.SwapUsage,
			HierarchicalMemoryLimit: m.Memory.UsageLimit,
		}
		total := m.Memory.Anon + m.Memory.File
		if m.Memory.InactiveFile < total {
			stats.Memory.TotalUsageTowardLimit = total - m.Memory.InactiveFile
		}
	}

	if m.Pids != nil {
		stats.Pid = garden.ContainerPidStat{Current: m.Pids.Current, Max: m.Pids.Limit}
	}

	return stats
}

func statsFromCgroup1(m *cgroup1stats.Metrics) gardener.StatsContainerMetrics {
	stats := gardener.StatsContainerMetrics{}

	if m.CPU != nil && m.CPU.Usage != nil {
		stats.CPU = garden.ContainerCPUStat{
			Usage:  m.CPU.Usage.Total,
			User:   m.CPU.Usage.User,
			System: m.CPU.Usage.Kernel,
		}
	}

	if m.Memory != nil {
		stats.Memory = garden.ContainerMemoryStat{
			Cache:             m.Memory.Cache,
			Rss:               m.Memory.RSS,
			TotalCache:        m.Memory.TotalCache,
			TotalRss:          m.Memory.TotalRSS,
			TotalInactiveFile: m.Memory.TotalInactiveFile,
			InactiveFile:      m.Memory.InactiveFile,
			ActiveFile:        m.Memory.ActiveFile,
			MappedFile:        m.Memory.MappedFile,
		}
		total := m.Memory.TotalRSS + m.Memory.TotalCache
		if m.Memory.TotalInactiveFile < total {
			stats.Memory.TotalUsageTowardLimit = total - m.Memory.TotalInactiveFile
		}
	}

	if m.Pids != nil {
		stats.Pid = garden.ContainerPidStat{Current: m.Pids.Current, Max: m.Pids.Limit}
	}

	return stats
}

// KillAll signals all processes of the container's task. It is used to stop
// containers run by VM based runtimes, whose processes are not visible in the
// host cgroup.
func (n *Nerd) KillAll(log lager.Logger, id string, kill bool) error {
	_, task, err := n.loadContainerAndTask(log, id)
	if err != nil {
		return err
	}

	signal := syscall.SIGTERM
	if kill {
		signal = syscall.SIGKILL
	}

	log.Debug("kill-all", lager.Data{"containerID": id, "signal": signal.String()})
	return task.Kill(n.context, signal, client.WithKillAll)
}
