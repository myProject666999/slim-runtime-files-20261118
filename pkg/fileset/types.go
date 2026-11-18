package fileset

import (
	"os"
	"time"
)

// Access is the strongest observed access semantics for a single path.
// The numeric order is the strength rank: Check < Read < Exec < Write.
// The string forms deliberately mirror pkg/report mondel OpType* values so an
// adapter at the caller boundary is a trivial AccessFromOp call.
type Access uint8

const (
	// AccessNone is the zero value; nothing has been observed.
	AccessNone Access = 0
	// AccessCheck is a metadata/stat-only access ("c").
	AccessCheck Access = 1
	// AccessRead is an open for reading ("r").
	AccessRead Access = 2
	// AccessExec is an execution access ("x").
	AccessExec Access = 3
	// AccessWrite is an open for writing ("w"), the strongest semantics and
	// the basis for later deciding whether write permission must be retained.
	AccessWrite Access = 4
)

// String renders an Access using the same single-letter wire values as report.OpType*.
func (a Access) String() string {
	switch a {
	case AccessCheck:
		return "c"
	case AccessRead:
		return "r"
	case AccessExec:
		return "x"
	case AccessWrite:
		return "w"
	default:
		return ""
	}
}

// AccessFromOp translates a report.OpType* wire value ("r","w","x","c") into an
// Access. An unknown or empty op yields AccessCheck and ok=false; the caller keeps
// the event (downgraded to a check) rather than dropping it or failing.
func AccessFromOp(op string) (Access, bool) {
	switch op {
	case "r":
		return AccessRead, true
	case "w":
		return AccessWrite, true
	case "x":
		return AccessExec, true
	case "c":
		return AccessCheck, true
	default:
		return AccessCheck, false
	}
}

// Kind classifies a recorded entry by object type, derived from an Lstat mode.
type Kind uint8

const (
	// KindMissing means an event was seen but nothing exists at that name under rootDir.
	KindMissing Kind = iota
	KindRegular
	KindDirectory
	KindSymlink
	KindDevice // char or block device
	KindNamedPipe
	KindSocket
	KindOther
)

// String returns a stable lowercase name for a Kind, useful in reports and tests.
func (k Kind) String() string {
	switch k {
	case KindMissing:
		return "missing"
	case KindRegular:
		return "file"
	case KindDirectory:
		return "dir"
	case KindSymlink:
		return "symlink"
	case KindDevice:
		return "device"
	case KindNamedPipe:
		return "fifo"
	case KindSocket:
		return "socket"
	default:
		return "other"
	}
}

// Meta holds the Lstat attributes of a recorded name. For a symlink these are the
// link's own attributes, never the target's. Uid/Gid are -1 when the platform
// cannot supply them; Ctime is zero on platforms that do not expose it. Atime is
// intentionally omitted because reading the file (including our own Lstat during a
// second synthesis) mutates it, making it nondeterministic across runs.
type Meta struct {
	Mode  os.FileMode
	Uid   int32
	Gid   int32
	Size  int64
	Inode uint64
	Mtime time.Time
	Ctime time.Time
}

// Entry is one member of the synthesized file set, keyed by its normalized
// container path in Result.Entries.
type Entry struct {
	// Path is the normalized, container-relative absolute path (always leading "/").
	Path string
	// Kind is the object type observed via Lstat of Path.
	Kind Kind
	// Access is the strongest observed semantics. Structural names (ancestor
	// directories, intermediate symlinks) carry AccessCheck unless they were
	// themselves the observed path.
	Access Access
	// Meta is populated from Lstat of Path and is zero-valued for KindMissing.
	Meta Meta
	// LinkTarget is the normalized container target; empty unless Kind == KindSymlink.
	LinkTarget string
	// Warnings holds non-fatal conditions recorded against this name (loop, dangling).
	Warnings []string
}

// Stats summarizes a synthesis run. Counts are deterministic functions of the set
// of unique observed paths, not of the number or order of events.
type Stats struct {
	Events         int
	UniquePaths    int
	Loops          int
	Dangling       int
	Missing        int
	PseudoFiltered int
	// Warnings mirrors every non-fatal warning emitted via the logger so tests can
	// assert on them without capturing log output.
	Warnings []string
}

// Result is the output of Builder.Build.
type Result struct {
	// Entries maps normalized container path to its Entry.
	Entries map[string]*Entry
	// Sorted lists the keys of Entries in lexical order for deterministic output.
	Sorted []string
	// Stats is a snapshot of the builder's counters at Build time.
	Stats Stats
}
