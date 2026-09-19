package docker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Usage is one sample of what a container uses.
type Usage struct {
	// CPUPercent is in percent of one core: 200 means two cores busy.
	CPUPercent float64
	// MemoryBytes is resident memory, without the page cache.
	MemoryBytes uint64
}

// Stats samples a running container's CPU and memory. It takes about a
// second, because the daemon collects two CPU readings.
func (e *Engine) Stats(ctx context.Context, name string) (Usage, error) {
	res, err := e.cli.ContainerStats(ctx, name, client.ContainerStatsOptions{Stream: false, IncludePreviousSample: true})
	if err != nil {
		return Usage{}, err
	}
	defer res.Body.Close()
	var st container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		return Usage{}, fmt.Errorf("stats %s: %w", name, err)
	}
	var u Usage
	cpuDelta := float64(st.CPUStats.CPUUsage.TotalUsage) - float64(st.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(st.CPUStats.SystemUsage) - float64(st.PreCPUStats.SystemUsage)
	cpus := float64(st.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(st.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpuDelta > 0 && sysDelta > 0 {
		u.CPUPercent = cpuDelta / sysDelta * cpus * 100
	}
	u.MemoryBytes = st.MemoryStats.Usage
	if cache, ok := st.MemoryStats.Stats["inactive_file"]; ok && cache < u.MemoryBytes {
		u.MemoryBytes -= cache
	} else if cache, ok := st.MemoryStats.Stats["total_inactive_file"]; ok && cache < u.MemoryBytes {
		u.MemoryBytes -= cache
	}
	return u, nil
}
