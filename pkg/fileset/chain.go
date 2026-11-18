package fileset

import (
	"os"
	"path"
)

// walk performs a kernel-style, component-by-component resolution of one normalized
// observed container path. It records every name it traverses — each ancestor
// directory, every intermediate symlink (even ones the process never opened), and
// the terminal object — using only Lstat and Readlink inside rootDir. It returns the
// terminal name (the last recorded entry, possibly KindMissing).
//
// Component-wise resolution is essential: Lstat of a full path follows symlinks in
// every component except the last, so calling Lstat("/a/link/file") would never
// reveal that "link" is itself a symlink. Inspecting one component at a time exposes
// every link on the chain.
func (b *Builder) walk(observed string) string {
	b.ensureRoot()

	queue := splitComponents(observed)
	resolved := "/" // already-resolved real directory (no symlinks), as a container path
	visited := map[string]struct{}{}
	hops := 0
	lastLink := "" // most recent symlink traversed (for dangling attribution)
	terminal := "/"

	for len(queue) > 0 {
		comp := queue[0]
		queue = queue[1:]

		cur := path.Join(resolved, comp)
		terminal = cur

		host, ok := b.hostOf(cur)
		if !ok {
			b.warnf("fileset: path %q escapes container root; stopping walk", cur)
			b.recordName(cur, KindMissing, Meta{}, "", AccessCheck)
			return terminal
		}

		fi, err := os.Lstat(host)
		if err != nil {
			if os.IsNotExist(err) {
				// Record the full missing suffix from here to the intended terminal,
				// so a dangling target whose ancestors are also absent still yields
				// the complete target name (every name on the chain is retained).
				missingName := cur
				remaining := append([]string(nil), queue...)
				for _, c := range remaining {
					missingName = path.Join(missingName, c)
					b.recordName(missingName, KindMissing, Meta{}, "", AccessCheck)
				}
				terminal = missingName
				b.recordMissing(missingName, hops, lastLink)
			} else {
				b.warnf("fileset: lstat %q failed: %v; retaining name without metadata", cur, err)
				b.recordName(cur, KindOther, Meta{}, "", AccessCheck)
			}
			return terminal
		}

		meta := lstatMeta(fi)
		if fi.Mode()&os.ModeSymlink == 0 {
			// Regular/dir/device/fifo/socket: terminal type for this component. Its
			// children (if a directory) are never listed, so no recursive ingestion.
			b.recordName(cur, kindOf(fi.Mode()), meta, "", AccessCheck)
			resolved = cur
			continue
		}

		// Symlink: inspect the link itself, never follow it via Stat.
		raw, err := os.Readlink(host)
		if err != nil {
			b.warnf("fileset: readlink %q failed: %v; retaining link", cur, err)
			b.recordName(cur, KindSymlink, meta, "", AccessCheck)
			return terminal
		}
		next := resolveTarget(cur, raw)
		b.recordName(cur, KindSymlink, meta, next, AccessCheck)
		lastLink = cur

		if _, seen := visited[cur]; seen {
			msg := "fileset: symlink loop detected at " + cur
			b.warnf("%s; retaining chain names and stopping", msg)
			b.attachWarning(cur, msg)
			b.stats.Loops++
			return terminal
		}
		visited[cur] = struct{}{}
		hops++
		if hops > b.maxHops {
			msg := "fileset: symlink hop cap exceeded at " + cur
			b.warnf("%s (max %d); retaining chain names and stopping", msg, b.maxHops)
			b.attachWarning(cur, msg)
			b.stats.Loops++
			return terminal
		}

		// Re-anchor at the container root and feed the FULL target back through the
		// component loop. This guarantees the target's own ancestor directories are
		// inspected and recorded even when they were not on the original path.
		resolved = "/"
		queue = append(splitComponents(next), queue...)
	}

	return terminal
}

// recordMissing records a name that does not exist under rootDir. When at least one
// symlink was traversed (hops > 0) it is the dangling target of a link; the warning
// is attributed to both the triggering link and the missing terminal. Otherwise it is
// a plain missing observed path. In all cases the names are retained as a signal.
func (b *Builder) recordMissing(p string, hops int, lastLink string) {
	b.recordName(p, KindMissing, Meta{}, "", AccessCheck)
	switch {
	case hops > 0:
		msg := "fileset: dangling symlink target " + p
		b.warnf("%s; retaining link names and stopping", msg)
		b.attachWarning(p, msg)
		if lastLink != "" && lastLink != p {
			b.attachWarning(lastLink, msg)
		}
		b.stats.Dangling++
	default:
		msg := "fileset: observed path missing from image: " + p
		b.warnf("%s", msg)
		b.attachWarning(p, msg)
		b.stats.Missing++
	}
}

// ensureRoot records the container root "/" as a directory with its own metadata, so
// the parent chain always has an apex even when an observed path's walk would not
// otherwise reach it. Idempotent via the single entries map.
func (b *Builder) ensureRoot() {
	if _, ok := b.entries["/"]; ok {
		return
	}
	host, ok := b.hostOf("/")
	if !ok {
		b.recordName("/", KindDirectory, Meta{}, "", AccessCheck)
		return
	}
	if fi, err := os.Lstat(host); err == nil {
		b.recordName("/", KindDirectory, lstatMeta(fi), "", AccessCheck)
		return
	}
	b.recordName("/", KindDirectory, Meta{}, "", AccessCheck)
}
