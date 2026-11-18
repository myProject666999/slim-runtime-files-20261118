package fileset

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- small root-fs construction helpers (paths are container paths inside root) ----

func mkdirp(tb testing.TB, root, container string) {
	tb.Helper()
	require.NoError(tb, os.MkdirAll(filepath.Join(root, container), 0o755))
}

func writeFile(tb testing.TB, root, container, content string) {
	tb.Helper()
	require.NoError(tb, os.MkdirAll(filepath.Join(root, filepath.Dir(container)), 0o755))
	require.NoError(tb, os.WriteFile(filepath.Join(root, container), []byte(content), 0o644))
}

func symlink(tb testing.TB, root, container, target string) {
	tb.Helper()
	require.NoError(tb, os.MkdirAll(filepath.Join(root, filepath.Dir(container)), 0o755))
	require.NoError(tb, os.Symlink(target, filepath.Join(root, container)))
}

func assertEntry(tb testing.TB, r *Result, p string, kind Kind) *Entry {
	tb.Helper()
	e, ok := r.Entries[p]
	if !assert.Truef(tb, ok, "expected entry %q; have %v", p, r.Sorted) {
		return nil
	}
	assert.Equalf(tb, kind, e.Kind, "entry %q kind", p)
	return e
}

func assertNotEntry(tb testing.TB, r *Result, p string) {
	tb.Helper()
	_, ok := r.Entries[p]
	assert.Falsef(tb, ok, "did not expect entry %q", p)
}

// TestTwoLevelRelativeChain: /a/l1 -> l2 -> ../etc/config; every link name, the
// final real file, and all ancestor directories enter the set.
func TestTwoLevelRelativeChain(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "/etc/config", "cfg")
	mkdirp(t, root, "/a")
	symlink(t, root, "/a/l2", "../etc/config")
	symlink(t, root, "/a/l1", "l2")

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/a/l1", "r"))
	r := b.Build()

	l1 := assertEntry(t, r, "/a/l1", KindSymlink)
	assert.Equal(t, "/a/l2", l1.LinkTarget)
	l2 := assertEntry(t, r, "/a/l2", KindSymlink)
	assert.Equal(t, "/etc/config", l2.LinkTarget)
	assertEntry(t, r, "/etc/config", KindRegular)
	// ancestors retained as directories
	assertEntry(t, r, "/a", KindDirectory)
	assertEntry(t, r, "/etc", KindDirectory)
	assertEntry(t, r, "/", KindDirectory)
}

// TestAbsoluteLinkReanchorsAtContainerRoot: an absolute target must resolve inside
// rootDir, never against the host root.
func TestAbsoluteLinkReanchorsAtContainerRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "/srv/v1/cfg", "v1")
	symlink(t, root, "/srv/current", "/srv/v1")

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/srv/current/cfg", "r"))
	r := b.Build()

	link := assertEntry(t, r, "/srv/current", KindSymlink)
	assert.Equal(t, "/srv/v1", link.LinkTarget)
	assertEntry(t, r, "/srv/v1", KindDirectory)
	assertEntry(t, r, "/srv/v1/cfg", KindRegular)
	assertEntry(t, r, "/srv", KindDirectory)
}

// TestSymlinkLoop: a -> ../loop/b -> a must not loop forever or fail the run; both
// names are retained, a loop is counted and warned.
func TestSymlinkLoop(t *testing.T) {
	root := t.TempDir()
	mkdirp(t, root, "/loop")
	symlink(t, root, "/loop/a", "../loop/b")
	symlink(t, root, "/loop/b", "a")

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/loop/a", "r"))
	r := b.Build()

	a := assertEntry(t, r, "/loop/a", KindSymlink)
	assert.Equal(t, "/loop/b", a.LinkTarget)
	bEntry := assertEntry(t, r, "/loop/b", KindSymlink)
	assert.Equal(t, "/loop/a", bEntry.LinkTarget)
	assert.GreaterOrEqual(t, r.Stats.Loops, 1)
	assert.NotEmpty(t, a.Warnings, "loop entry carries a warning")
	assert.NotEmpty(t, r.Stats.Warnings)
}

// TestSelfLoop: a -> a is detected via the visited set.
func TestSelfLoop(t *testing.T) {
	root := t.TempDir()
	mkdirp(t, root, "/sl")
	symlink(t, root, "/sl/self", "self")

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/sl/self", "r"))
	r := b.Build()

	assertEntry(t, r, "/sl/self", KindSymlink)
	assert.GreaterOrEqual(t, r.Stats.Loops, 1)
}

// TestDanglingLink: the link name and its missing target are retained; dangling is
// counted and warned, the walk stops cleanly.
func TestDanglingLink(t *testing.T) {
	root := t.TempDir()
	mkdirp(t, root, "/etc")
	symlink(t, root, "/etc/missing", "/var/empty/x")

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/etc/missing", "r"))
	r := b.Build()

	link := assertEntry(t, r, "/etc/missing", KindSymlink)
	assert.Equal(t, "/var/empty/x", link.LinkTarget)
	assertEntry(t, r, "/var/empty/x", KindMissing)
	assert.Equal(t, 1, r.Stats.Dangling)
	assert.NotEmpty(t, link.Warnings)
}

// TestPlainMissingPath: an event for a nonexistent regular path is retained as
// missing (distinct from a dangling symlink) and counted as Missing.
func TestPlainMissingPath(t *testing.T) {
	root := t.TempDir()
	mkdirp(t, root, "/etc")

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/etc/nope", "r"))
	r := b.Build()

	assertEntry(t, r, "/etc/nope", KindMissing)
	assert.Equal(t, 1, r.Stats.Missing)
	assert.Equal(t, 0, r.Stats.Dangling)
}

// TestDirectorySymlinkNotRecursive: resolving a symlink to a directory must retain
// the link and the target dir but never ingest the directory's other children.
func TestDirectorySymlinkNotRecursive(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "/var/log/app.log", "log")
	writeFile(t, root, "/var/log/secret.log", "secret") // sibling must NOT be pulled in
	symlink(t, root, "/logs", "/var/log")

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/logs/app.log", "r"))
	r := b.Build()

	assertEntry(t, r, "/logs", KindSymlink)
	assertEntry(t, r, "/var/log", KindDirectory)
	assertEntry(t, r, "/var/log/app.log", KindRegular)
	assertNotEntry(t, r, "/var/log/secret.log")
}

// TestParentMetadataPreserved: ancestor directories keep their real permission bits.
func TestParentMetadataPreserved(t *testing.T) {
	root := t.TempDir()
	mkdirp(t, root, "/secure")
	require.NoError(t, os.Chmod(filepath.Join(root, "/secure"), 0o700))
	writeFile(t, root, "/secure/data", "x")
	require.NoError(t, os.Chmod(filepath.Join(root, "/secure/data"), 0o600))

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/secure/data", "r"))
	r := b.Build()

	dir := assertEntry(t, r, "/secure", KindDirectory)
	assert.Equal(t, os.FileMode(0o700), dir.Meta.Mode.Perm())
	file := assertEntry(t, r, "/secure/data", KindRegular)
	assert.Equal(t, os.FileMode(0o600), file.Meta.Mode.Perm())
}

// TestReadThenWriteKeepsStronger: write wins on the observed name and its terminal
// file; intermediate symlinks and ancestor dirs stay at check.
func TestReadThenWriteKeepsStronger(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "/etc/config", "c")
	symlink(t, root, "/etc/link", "config")

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/etc/link", "r"))
	require.NoError(t, b.Add("/etc/link", "w"))
	r := b.Build()

	assert.Equal(t, AccessWrite, r.Entries["/etc/link"].Access)
	assert.Equal(t, AccessWrite, r.Entries["/etc/config"].Access)
	assert.Equal(t, AccessCheck, r.Entries["/etc"].Access)
}

// TestRootEntryAndNormalization: a redundant/separator-laden event normalizes onto a
// single entry, and "/" is always present.
func TestRootEntryAndNormalization(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "/etc/config", "c")

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/etc//./config/", "r"))
	r := b.Build()

	assertEntry(t, r, "/etc/config", KindRegular)
	assertNotEntry(t, r, "/etc//./config/")
	assertEntry(t, r, "/", KindDirectory)
}

// TestMetadataTimeRoundTrip: mtime set on a file is preserved on its entry.
func TestMetadataTimeRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "/etc/config", "c")
	want := time.Date(2021, time.Month(7), 4, 12, 30, 45, 0, time.UTC)
	host := filepath.Join(root, "/etc/config")
	require.NoError(t, os.Chtimes(host, want, want))

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/etc/config", "r"))
	r := b.Build()

	e := assertEntry(t, r, "/etc/config", KindRegular)
	assert.True(t, e.Meta.Mtime.Equal(want), "mtime=%v want=%v", e.Meta.Mtime, want)
}

// TestMetadataIdentityPopulated: uid/gid/inode are populated from stat on Unix.
func TestMetadataIdentityPopulated(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "/etc/config", "c")

	b, err := New(root)
	require.NoError(t, err)
	require.NoError(t, b.Add("/etc/config", "r"))
	r := b.Build()

	e := assertEntry(t, r, "/etc/config", KindRegular)
	assert.GreaterOrEqual(t, e.Meta.Uid, int32(0))
	assert.GreaterOrEqual(t, e.Meta.Gid, int32(0))
	assert.NotZero(t, e.Meta.Inode)
}

// TestNonAbsolutePathRejected: relative event paths return an error and record nothing.
func TestNonAbsolutePathRejected(t *testing.T) {
	root := t.TempDir()
	b, err := New(root)
	require.NoError(t, err)

	err = b.Add("etc/config", "r")
	assert.Error(t, err)
	r := b.Build()
	_, hasRoot := r.Entries["/"]
	assert.False(t, hasRoot, "no walk should run, so even root must not be synthesized")
}
