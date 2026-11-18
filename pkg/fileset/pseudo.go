package fileset

import (
	"os"
	"strings"
)

// Class identifies the pseudo-filesystem category (or ordinary image content) of a
// container path. The numeric values are stable and used by tests.
type Class uint8

const (
	ClassImage       Class = iota // ordinary in-image path
	ClassRoot                     // "/" itself
	ClassPseudoMount              // /proc, /sys, /dev mount points
	ClassProcEntry
	ClassSysEntry
	ClassDevEntry
)

// Action is the retain/filter verdict.
type Action uint8

const (
	ActionRetain Action = iota
	ActionFilter
)

// Decision is the auditable verdict for one path. Reason is a stable, exported
// constant so tests and reviewers can assert exactly which rule fired.
type Decision struct {
	Class  Class
	Action Action
	Reason string
}

// Reviewable reason strings. The full rule table with tradeoffs lives in DESIGN.md.
const (
	ReasonRootRetained         = "root: image root is a real tar entry and the metadata apex"
	ReasonPseudoMountRetained  = "pseudo-mount: empty mount-point directory present in the image anchors exception children"
	ReasonProcFiltered         = "proc: kernel-generated per-pid object is not image content"
	ReasonProcLinkRetained     = "proc-exception: symlink target resolves to an existing in-image path"
	ReasonProcLinkFiltered     = "proc: symlink target is kernel-only or absent (socket/pipe/pts/nonexistent)"
	ReasonProcLinkReclassified = "proc: symlink target reclassifies into /proc or /sys and is filtered"
	ReasonSysFiltered          = "sys: sysfs objects are kernel-owned and never image payload"
	ReasonDevBakedRetained     = "dev-exception: node is physically present in the image (pre-baked)"
	ReasonDevRuntimeFiltered   = "dev: runtime pseudo/tmpfs entry absent from the image"
	ReasonPseudoExceptionsOff  = "pseudo: exceptions disabled, all /proc /sys /dev content filtered"
	ReasonImageRetained        = "image: ordinary image content"
)

// Pseudo mount points and their entry classes.
var (
	pseudoMounts = map[string]Class{
		"/proc": ClassProcEntry,
		"/sys":  ClassSysEntry,
		"/dev":  ClassDevEntry,
	}
)

// isPseudoKernelPath reports whether p is the mount point or a descendant of
// /proc or /sys (the kernel virtual filesystems). /dev is handled separately
// because its baked-node exception is presence-based rather than link-based.
func isPseudoKernelPath(p string) bool {
	return p == "/proc" || p == "/sys" ||
		hasPrefixSegment(p, "/proc/") || hasPrefixSegment(p, "/sys/")
}

func hasPrefixSegment(p, prefix string) bool {
	return len(p) >= len(prefix) && p[:len(prefix)] == prefix
}

// isKernelObject reports whether a readlink result names a kernel pseudo object
// rather than a filesystem path (e.g. "socket:[1]", "pipe:[2]", "anon_inode:[..]").
func isKernelObject(target string) bool {
	for _, prefix := range []string{"socket:", "pipe:", "anon_inode:"} {
		if strings.HasPrefix(target, prefix) {
			return true
		}
	}
	return false
}

// classify applies the ordered, reviewable rule table and returns a verdict. It
// performs only Lstat/Readlink inspections inside rootDir. The retain/filter table
// is mirrored verbatim in DESIGN.md.
func (b *Builder) classify(p string) Decision {
	if p == "/" {
		return Decision{ClassRoot, ActionRetain, ReasonRootRetained}
	}

	if _, ok := pseudoMounts[p]; ok {
		return Decision{ClassPseudoMount, ActionRetain, ReasonPseudoMountRetained}
	}

	switch {
	case p == "/proc" || hasPrefixSegment(p, "/proc/"):
		if !b.pseudoExceptions {
			return Decision{ClassProcEntry, ActionFilter, ReasonPseudoExceptionsOff}
		}
		return b.classifyProc(p)
	case p == "/sys" || hasPrefixSegment(p, "/sys/"):
		return Decision{ClassSysEntry, ActionFilter, ReasonSysFiltered}
	case p == "/dev" || hasPrefixSegment(p, "/dev/"):
		if !b.pseudoExceptions {
			return Decision{ClassDevEntry, ActionFilter, ReasonPseudoExceptionsOff}
		}
		return b.classifyDev(p)
	default:
		return Decision{ClassImage, ActionRetain, ReasonImageRetained}
	}
}

// classifyDev implements exception (a): a /dev path is retained exactly when the
// image physically contains it (Lstat succeeds under rootDir). A runtime tmpfs/
// pseudo device has no host counterpart and is filtered.
func (b *Builder) classifyDev(p string) Decision {
	host, ok := b.hostOf(p)
	if !ok {
		return Decision{ClassDevEntry, ActionFilter, ReasonDevRuntimeFiltered}
	}
	if _, err := os.Lstat(host); err == nil {
		return Decision{ClassDevEntry, ActionRetain, ReasonDevBakedRetained}
	}
	return Decision{ClassDevEntry, ActionFilter, ReasonDevRuntimeFiltered}
}

// classifyProc implements exception (b): retain only a /proc symlink whose target
// resolves to an existing in-image path. Ordinary proc objects, and links whose
// targets are kernel-only, absent, or reclassify back into /proc or /sys, are filtered.
func (b *Builder) classifyProc(p string) Decision {
	host, ok := b.hostOf(p)
	if !ok {
		return Decision{ClassProcEntry, ActionFilter, ReasonProcFiltered}
	}
	fi, err := os.Lstat(host)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		// Rule 3: any non-symlink /proc object (or one absent from the image) is filtered.
		return Decision{ClassProcEntry, ActionFilter, ReasonProcFiltered}
	}

	raw, err := os.Readlink(host)
	if err != nil {
		return Decision{ClassProcEntry, ActionFilter, ReasonProcFiltered}
	}

	// Kernel-only pseudo objects are exposed by /proc/self/fd links as non-path
	// strings ("socket:[123]", "pipe:[456]", "anon_inode:[...]"). They can never
	// name an image file, so filter them before any path resolution.
	if isKernelObject(raw) {
		return Decision{ClassProcEntry, ActionFilter, ReasonProcLinkFiltered}
	}

	target := resolveTarget(p, raw)

	// A link that points back into a kernel virtual filesystem cannot launder an
	// exception: reclassify the target through the same table.
	if isPseudoKernelPath(target) {
		return Decision{ClassProcEntry, ActionFilter, ReasonProcLinkReclassified}
	}

	targetHost, ok := b.hostOf(target)
	if !ok {
		return Decision{ClassProcEntry, ActionFilter, ReasonProcLinkFiltered}
	}
	if _, err := os.Lstat(targetHost); err != nil {
		return Decision{ClassProcEntry, ActionFilter, ReasonProcLinkFiltered}
	}
	return Decision{ClassProcEntry, ActionRetain, ReasonProcLinkRetained}
}
