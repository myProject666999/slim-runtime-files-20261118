package fileset

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPseudoOrdinaryFiltered: ordinary /proc and /sys objects and an absent /dev
// runtime entry are filtered and accounted for.
func TestPseudoOrdinaryFiltered(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "/proc/cpuinfo", "cpu") // ordinary proc text file
	mkdirp(t, root, "/proc/1")
	writeFile(t, root, "/proc/1/cmdline", "cmd")     // per-pid object
	writeFile(t, root, "/sys/class/net/eth0", "eth") // sysfs object
	// /dev/ptmx intentionally NOT created on disk -> runtime pseudo entry

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/proc/cpuinfo", "r"))
	require.NoError(t, b.Add("/proc/1/cmdline", "r"))
	require.NoError(t, b.Add("/sys/class/net/eth0", "r"))
	require.NoError(t, b.Add("/dev/ptmx", "r"))
	r := b.Build()

	assertNotEntry(t, r, "/proc/cpuinfo")
	assertNotEntry(t, r, "/proc/1/cmdline")
	assertNotEntry(t, r, "/sys/class/net/eth0")
	assertNotEntry(t, r, "/dev/ptmx")
	assert.Equal(t, 4, r.Stats.PseudoFiltered)

	// Repeated filtered events stay filtered and remain O(1) (no entries synthesized).
	require.NoError(t, b.Add("/proc/cpuinfo", "r"))
	r2 := b.Build()
	assert.Equal(t, 5, r2.Stats.PseudoFiltered)
	assert.Empty(t, r2.Entries)
}

// TestPseudoMountPointsRetained: the mount-point directories themselves are kept.
func TestPseudoMountPointsRetained(t *testing.T) {
	root := t.TempDir()
	mkdirp(t, root, "/proc")
	mkdirp(t, root, "/sys")
	mkdirp(t, root, "/dev")

	b, err := New(root)
	require.NoError(t, err)
	// Touching the mount point directly exercises the retain rule.
	for _, p := range []string{"/proc", "/sys", "/dev"} {
		d := b.classify(p)
		assert.Equal(t, ActionRetain, d.Action)
		assert.Equal(t, ReasonPseudoMountRetained, d.Reason)
	}
}

// TestDevBakedNodeException: a /dev path physically present in the image is retained
// (exception a); a missing one is filtered.
func TestDevBakedNodeException(t *testing.T) {
	root := t.TempDir()
	mkdirp(t, root, "/dev")
	writeFile(t, root, "/dev/shim", "static-shim") // image ships a real file under /dev

	b, err := New(root)
	require.NoError(t, err)

	retain := b.classify("/dev/shim")
	assert.Equal(t, ActionRetain, retain.Action)
	assert.Equal(t, ReasonDevBakedRetained, retain.Reason)

	filtered := b.classify("/dev/runtime0")
	assert.Equal(t, ActionFilter, filtered.Action)
	assert.Equal(t, ReasonDevRuntimeFiltered, filtered.Reason)

	// End-to-end: the baked node is synthesized with real metadata, the runtime one is not.
	require.NoError(t, b.Add("/dev/shim", "r"))
	require.NoError(t, b.Add("/dev/runtime0", "r"))
	r := b.Build()
	shim := assertEntry(t, r, "/dev/shim", KindRegular)
	assert.Equal(t, AccessRead, shim.Access)
	assertNotEntry(t, r, "/dev/runtime0")
}

// TestDevBakedCharNode exercises a real device node when the test can create one
// (mknod needs privilege); otherwise it skips. The presence logic is already covered
// unprivileged by the static-file case above.
func TestDevBakedCharNode(t *testing.T) {
	root := t.TempDir()
	mkdirp(t, root, "/dev")
	node := filepath.Join(root, "/dev/nullish")
	if err := syscall.Mknod(node, syscall.S_IFCHR|0o666, 0); err != nil {
		t.Skipf("mknod requires privilege, skipping device-node case: %v", err)
	}

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/dev/nullish", "r"))
	r := b.Build()

	e := assertEntry(t, r, "/dev/nullish", KindDevice)
	assert.NotZero(t, e.Meta.Mode&os.ModeDevice)
}

// TestProcSymlinkIntoImageException: a /proc symlink to an existing in-image file is
// retained and its target chain pulled in (exception b).
func TestProcSymlinkIntoImageException(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "/var/log/app.log", "log")
	mkdirp(t, root, "/proc/10/fd")
	// Materialize what the kernel would expose: an fd link into the image.
	symlink(t, root, "/proc/10/fd/3", "/var/log/app.log")

	b, err := New(root)
	require.NoError(t, err)

	d := b.classify("/proc/10/fd/3")
	assert.Equal(t, ActionRetain, d.Action)
	assert.Equal(t, ReasonProcLinkRetained, d.Reason)

	require.NoError(t, b.Add("/proc/10/fd/3", "r"))
	r := b.Build()

	link := assertEntry(t, r, "/proc/10/fd/3", KindSymlink)
	assert.Equal(t, "/var/log/app.log", link.LinkTarget)
	assertEntry(t, r, "/var/log/app.log", KindRegular)
	assertEntry(t, r, "/var/log", KindDirectory)
}

// TestProcSymlinkKernelOnlyFiltered: /proc links to socket:/pipe:/sys targets are
// filtered (the non-retaining side of exception b).
func TestProcSymlinkKernelOnlyFiltered(t *testing.T) {
	root := t.TempDir()
	mkdirp(t, root, "/proc/10/fd")
	mkdirp(t, root, "/sys/kernel")
	writeFile(t, root, "/sys/kernel/x", "k")
	symlink(t, root, "/proc/10/fd/4", "socket:[1]")    // kernel-only, absent on disk
	symlink(t, root, "/proc/10/fd/5", "/sys/kernel/x") // reclassifies into sysfs

	b, err := New(root)
	require.NoError(t, err)

	d4 := b.classify("/proc/10/fd/4")
	assert.Equal(t, ActionFilter, d4.Action)
	assert.Equal(t, ReasonProcLinkFiltered, d4.Reason)

	d5 := b.classify("/proc/10/fd/5")
	assert.Equal(t, ActionFilter, d5.Action)
	assert.Equal(t, ReasonProcLinkReclassified, d5.Reason)
}

// TestPseudoExceptionsDisabled: turning exceptions off filters all pseudo content
// including baked /dev nodes and into-image /proc links.
func TestPseudoExceptionsDisabled(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "/dev/shim", "x")
	writeFile(t, root, "/var/log/app.log", "log")
	mkdirp(t, root, "/proc/10/fd")
	symlink(t, root, "/proc/10/fd/3", "/var/log/app.log")

	b, err := New(root, WithPseudoExceptions(false))
	require.NoError(t, err)
	require.NoError(t, b.Add("/dev/shim", "r"))
	require.NoError(t, b.Add("/proc/10/fd/3", "r"))
	r := b.Build()

	assertNotEntry(t, r, "/dev/shim")
	assertNotEntry(t, r, "/proc/10/fd/3")
	assert.Equal(t, 2, r.Stats.PseudoFiltered)
}
