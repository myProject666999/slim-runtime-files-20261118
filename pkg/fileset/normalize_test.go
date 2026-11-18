package fileset

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalize(t *testing.T) {
	tt := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", "/etc/config", "/etc/config", true},
		{"redundant separators", "/etc//config///", "/etc/config", true},
		{"current dir segments", "/etc/./config", "/etc/config", true},
		{"parent climb inside tree", "/a/b/../c", "/a/c", true},
		{"parent climb clamped at root", "/a/../../etc", "/etc", true},
		{"root stays root", "/", "/", true},
		{"trailing slash", "/etc/", "/etc", true},
		{"relative rejected", "etc/config", "", false},
		{"empty rejected", "", "", false},
		{"dot rejected", ".", "", false},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalize(tc.in)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolveTarget(t *testing.T) {
	tt := []struct {
		name   string
		link   string
		target string
		want   string
	}{
		{"relative resolves at link dir", "/a/l1", "l2", "/a/l2"},
		{"relative parent climb", "/a/b/l", "../etc/c", "/a/etc/c"},
		{"absolute re-anchored at container root", "/srv/current", "/srv/v1", "/srv/v1"},
		{"absolute with redundant bits", "/x/l", "/v//./w", "/v/w"},
		{"relative dot", "/a/l", "./f", "/a/f"},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveTarget(tc.link, tc.target))
		})
	}
}

func TestSplitComponents(t *testing.T) {
	assert.Nil(t, splitComponents("/"))
	assert.Equal(t, []string{"a"}, splitComponents("/a"))
	assert.Equal(t, []string{"a", "b", "c"}, splitComponents("/a/b/c"))
}

func TestAccessRankAndMerge(t *testing.T) {
	// read then write keeps the stronger (write) semantics, order-independent.
	assert.Equal(t, AccessWrite, merge(AccessRead, AccessWrite))
	assert.Equal(t, AccessWrite, merge(AccessWrite, AccessRead))
	assert.Equal(t, AccessExec, merge(AccessRead, AccessExec))
	assert.Equal(t, AccessRead, merge(AccessRead, AccessCheck))
	assert.Equal(t, AccessRead, merge(AccessNone, AccessRead))
}

func TestAccessFromOp(t *testing.T) {
	cases := map[string]Access{"r": AccessRead, "w": AccessWrite, "x": AccessExec, "c": AccessCheck}
	for op, want := range cases {
		got, ok := AccessFromOp(op)
		assert.True(t, ok)
		assert.Equal(t, want, got)
	}
	got, ok := AccessFromOp("z")
	assert.False(t, ok)
	assert.Equal(t, AccessCheck, got)
}

func TestHostOfContainment(t *testing.T) {
	b, err := New(t.TempDir())
	require.NoError(t, err)

	host, ok := b.hostOf("/etc/config")
	assert.True(t, ok)
	assert.Contains(t, host, b.rootDir)

	// Force an already-escaped host join (bypassing normalize, as a malformed symlink
	// target could produce) and confirm the fence refuses it instead of inspecting a
	// path outside the container root.
	escaped := filepath.Join(b.rootDir, "..", "outside")
	rel, rerr := filepath.Rel(b.rootDir, escaped)
	require.NoError(t, rerr)
	assert.True(t, rel == ".." || hasPrefixSegment(rel, ".."+string(os.PathSeparator)))
	_, ok = guardedHostOf(b, "/../outside")
	assert.False(t, ok)
}

// guardedHostOf mirrors the containment computation in Builder.hostOf so the test can
// feed an un-normalized, escaping relative segment.
func guardedHostOf(b *Builder, p string) (string, bool) {
	host := filepath.Join(b.rootDir, p)
	rel, err := filepath.Rel(b.rootDir, host)
	if err != nil || rel == ".." || hasPrefixSegment(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return host, true
}
