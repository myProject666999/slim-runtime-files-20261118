// Package fileset synthesizes the minimal set of image files required at runtime
// from a stream of container file-access events.
//
// # Model
//
// A Builder is bound to a host-side root directory (rootDir) that holds the
// container's root filesystem extracted from the image. Every path observed in an
// event is a *container* path (always absolute, beginning with "/"). The package
// never interprets such a path against the host root: the host path used for an
// inspection is always filepath.Join(rootDir, containerPath), and every join is
// fenced so a path cannot escape rootDir.
//
// Symlinks are expanded level by level against the container namespace: a relative
// link target resolves against the directory that contains the link, while an
// absolute target re-anchors at the container root "/". Resolution is performed
// component by component so that intermediate symlinks (the names a process never
// opens directly) are discovered and retained, together with the final real file
// and every ancestor directory and its metadata.
//
// # Read-only by contract
//
// The only filesystem operations this package performs are os.Lstat and
// os.Readlink on paths inside rootDir. It never follows a link with Stat, never
// calls EvalSymlinks, and never opens or lists a directory (no ReadDir/Walk).
// Consequently it cannot recursively ingest a directory's contents and never
// mutates or copies anything.
//
// A Builder is not safe for concurrent use; callers must fan events in from a
// single goroutine or synchronize before calling Add/AddAccess.
package fileset
