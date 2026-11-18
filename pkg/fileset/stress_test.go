package fileset

import (
	"math/rand/v2"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const stressEventCount = 100_000

// buildStressRoot creates a small root fs spanning ~20 unique paths with every link
// shape: relative/absolute chains, a loop, a dangling link, and a directory symlink.
func buildStressRoot(tb testing.TB) string {
	tb.Helper()
	root := tb.TempDir()

	writeFile(tb, root, "/etc/config", "c")
	writeFile(tb, root, "/etc/hosts", "h")
	writeFile(tb, root, "/bin/tool", "t")
	writeFile(tb, root, "/var/log/app.log", "l")
	writeFile(tb, root, "/var/log/other.log", "o")
	writeFile(tb, root, "/srv/v1/cfg", "v1")
	writeFile(tb, root, "/srv/v2/cfg", "v2")

	// relative two-level chain: /lib/l1 -> l2 -> ../etc/config
	symlink(tb, root, "/lib/l2", "../etc/config")
	symlink(tb, root, "/lib/l1", "l2")
	// absolute link
	symlink(tb, root, "/srv/current", "/srv/v1")
	// directory symlink (sibling must not be ingested)
	symlink(tb, root, "/logs", "/var/log")
	// loop
	symlink(tb, root, "/loop/a", "b")
	symlink(tb, root, "/loop/b", "a")
	// dangling
	symlink(tb, root, "/etc/dangling", "/var/empty/x")

	return root
}

// stressObserved are the non-pseudo observed paths, with the strongest op each will
// reach in the shuffled run.
var stressObserved = []struct {
	path string
	op   string
}{
	{"/etc/config", "w"},
	{"/etc/hosts", "r"},
	{"/bin/tool", "x"},
	{"/var/log/app.log", "w"},
	{"/lib/l1", "r"},
	{"/srv/current/cfg", "r"},
	{"/logs/app.log", "r"},
	{"/loop/a", "r"},
	{"/etc/dangling", "r"},
}

// stressFiltered are pseudo paths that are always rejected, interleaved at volume.
var stressFiltered = []string{
	"/proc/cpuinfo",
	"/proc/1/cmdline",
	"/sys/class/net/eth0",
	"/dev/runtime9",
}

func TestStressDedupAndOrderIndependence(t *testing.T) {
	root := buildStressRoot(t)
	allOps := []string{"c", "r", "x", "w"}

	// Baseline over the SAME root: feed every op once per unique path so the merged
	// access lands at the strongest level, matching the op-rotating stressed run.
	base, err := New(root)
	require.NoError(t, err)
	for _, e := range stressObserved {
		for _, op := range allOps {
			require.NoError(t, base.Add(e.path, op))
		}
	}
	for _, p := range stressFiltered {
		require.NoError(t, base.Add(p, "r"))
	}
	baseline := base.Build()

	// Stressed: 100k out-of-order, rotating-op, duplicate events plus pseudo noise.
	stressed, err := New(root)
	require.NoError(t, err)

	rng := rand.New(rand.NewPCG(42, 99))
	start := time.Now()
	for i := 0; i < stressEventCount; i++ {
		if i%5 == 0 {
			p := stressFiltered[rng.IntN(len(stressFiltered))]
			require.NoError(t, stressed.Add(p, "r"))
			continue
		}
		e := stressObserved[rng.IntN(len(stressObserved))]
		// Rotate ops so the same path is seen read before write in some orders.
		op := allOps[rng.IntN(len(allOps))]
		require.NoError(t, stressed.Add(e.path, op))
	}
	elapsed := time.Since(start)

	r := stressed.Build()

	// Deterministic, order-independent result equal to the single-input baseline.
	assert.Equal(t, baseline.Entries, r.Entries)
	assert.Equal(t, baseline.Sorted, r.Sorted)
	assert.Equal(t, baseline.Stats.Loops, r.Stats.Loops)
	assert.Equal(t, baseline.Stats.Dangling, r.Stats.Dangling)
	assert.Equal(t, baseline.Stats.Missing, r.Stats.Missing)
	assert.Equal(t, baseline.Stats.UniquePaths, r.Stats.UniquePaths)
	assert.Equal(t, len(stressObserved), r.Stats.UniquePaths)
	assert.Equal(t, stressEventCount, r.Stats.Events)
	assert.Positive(t, r.Stats.PseudoFiltered)

	// Threshold: 100k events handled well within budget even under -count 10.
	assert.Less(t, elapsed, 300*time.Millisecond, "100k Add calls should finish under 300ms")
}

func TestStressMemoryBoundedByUniquePaths(t *testing.T) {
	root := buildStressRoot(t)
	b, err := New(root)
	require.NoError(t, err)

	// Prime the builder so every unique path is already synthesized.
	for _, e := range stressObserved {
		require.NoError(t, b.Add(e.path, e.op))
	}
	for _, p := range stressFiltered {
		require.NoError(t, b.Add(p, "r"))
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	for i := 0; i < stressEventCount; i++ {
		e := stressObserved[i%len(stressObserved)]
		require.NoError(t, b.Add(e.path, "r"))
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	// A duplicate-only batch must not accumulate state: effectively flat heap.
	assert.Less(t, growth, int64(2<<20), "duplicate event batch grew heap by %d bytes", growth)
}

func TestDuplicateAddIsAllocationFree(t *testing.T) {
	root := buildStressRoot(t)
	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/etc/config", "r"))

	allocs := testing.AllocsPerRun(100, func() {
		b.AddAccess("/etc/config", AccessRead) // equal/weaker op on a known path
	})
	assert.Equal(t, 0.0, allocs)
}

func BenchmarkAdd(b *testing.B) {
	root := buildStressRoot(b)
	builder, err := New(root)
	if err != nil {
		b.Fatal(err)
	}
	for _, e := range stressObserved {
		if err := builder.Add(e.path, e.op); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := stressObserved[i%len(stressObserved)]
		_ = builder.Add(e.path, "r")
	}
}
