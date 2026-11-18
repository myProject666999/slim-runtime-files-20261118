package fileset

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	log "github.com/sirupsen/logrus"
)

// defaultMaxHops matches Linux MAXSYMLINKS: the kernel returns ELOOP once a path
// resolution traverses more than this many symlinks.
const defaultMaxHops = 40

// Builder incrementally synthesizes a minimal file set from access events bound to
// a single extracted container root. It is single-goroutine by contract.
type Builder struct {
	rootDir          string
	maxHops          int
	pseudoExceptions bool
	logger           log.FieldLogger

	// entries is the single backing store; its size is bounded by the number of
	// unique paths, independent of the number of events ingested.
	entries map[string]*Entry
	// done records observed paths whose component walk has already run, so repeated
	// events never re-traverse the filesystem or re-increment anomaly counters.
	done map[string]struct{}
	// filtered records observed paths the classifier rejected (memoized).
	filtered map[string]struct{}
	// terminal maps an observed path to the final name its walk reached, so a
	// stronger later access can be merged onto both the observed name and its target.
	terminal map[string]string

	stats Stats
}

// Option customizes a Builder at construction.
type Option func(*Builder)

// WithMaxHops overrides the symlink traversal cap (default 40). Non-positive values
// fall back to the default.
func WithMaxHops(n int) Option {
	return func(b *Builder) { b.maxHops = n }
}

// WithPseudoExceptions toggles the /dev baked-node and /proc into-image symlink
// exceptions (default true). When false, all /proc, /sys and /dev content is filtered.
func WithPseudoExceptions(on bool) Option {
	return func(b *Builder) { b.pseudoExceptions = on }
}

// WithLogger supplies a structured logger; the default is logrus.StandardLogger().
func WithLogger(entry *log.Entry) Option {
	return func(b *Builder) {
		if entry != nil {
			b.logger = entry
		}
	}
}

// New validates rootDir (the extracted container root) and returns a Builder.
func New(rootDir string, opts ...Option) (*Builder, error) {
	abs, err := filepath.Abs(filepath.Clean(rootDir))
	if err != nil {
		return nil, fmt.Errorf("fileset: resolve root dir: %w", err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("fileset: root dir %q: %w", abs, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("fileset: root dir %q is not a directory", abs)
	}

	b := &Builder{
		rootDir:          abs,
		maxHops:          defaultMaxHops,
		pseudoExceptions: true,
		logger:           log.StandardLogger(),
		entries:          map[string]*Entry{},
		done:             map[string]struct{}{},
		filtered:         map[string]struct{}{},
		terminal:         map[string]string{},
	}
	for _, opt := range opts {
		opt(b)
	}
	if b.maxHops <= 0 {
		b.maxHops = defaultMaxHops
	}
	return b, nil
}

// Add ingests one event given a report.OpType*-style op string ("r","w","x","c").
// An unknown op is downgraded to a check with a warning. A non-absolute path is an
// error and records nothing.
func (b *Builder) Add(containerPath, op string) error {
	if _, ok := normalize(containerPath); !ok {
		return fmt.Errorf("fileset: event path must be a non-empty absolute container path: %q", containerPath)
	}
	acc, known := AccessFromOp(op)
	if !known {
		b.warnf("fileset: unknown op %q on %q; treating as check", op, containerPath)
	}
	b.AddAccess(containerPath, acc)
	return nil
}

// AddAccess ingests one event with an explicit Access level. It is the lower-level
// entry point used by Add and by callers that already translate their own events.
func (b *Builder) AddAccess(containerPath string, acc Access) {
	p, ok := normalize(containerPath)
	if !ok {
		b.warnf("fileset: reject non-absolute/empty event path %q", containerPath)
		return
	}
	b.stats.Events++

	// Memoized filter verdict: no classification cost or counter drift on repeats.
	if _, skip := b.filtered[p]; skip {
		b.stats.PseudoFiltered++
		return
	}

	if _, walked := b.done[p]; !walked {
		if d := b.classify(p); d.Action == ActionFilter {
			b.filtered[p] = struct{}{}
			b.stats.PseudoFiltered++
			return
		}
		term := b.walk(p)
		b.done[p] = struct{}{}
		b.terminal[p] = term
		b.stats.UniquePaths++
	}

	b.applyObserved(p, acc)
}

// applyObserved merges the event's access onto the observed name and the terminal
// object its chain reaches, leaving structural ancestors and intermediate symlinks
// at AccessCheck. Merging (rather than assigning) keeps the strongest semantics and
// makes the result independent of event order.
func (b *Builder) applyObserved(observedPath string, acc Access) {
	if e, ok := b.entries[observedPath]; ok {
		e.Access = merge(e.Access, acc)
	}
	if term, ok := b.terminal[observedPath]; ok && term != observedPath {
		if e, ok := b.entries[term]; ok {
			e.Access = merge(e.Access, acc)
		}
	}
}

// recordName upserts a name at the given structural access. Concrete Lstat data is
// only used to fill an entry previously recorded as a placeholder, so repeated
// upserts never clobber good metadata.
func (b *Builder) recordName(p string, kind Kind, meta Meta, linkTarget string, acc Access) *Entry {
	e, ok := b.entries[p]
	if !ok {
		return b.putEntry(&Entry{Path: p, Kind: kind, Access: acc, Meta: meta, LinkTarget: linkTarget})
	}
	e.Access = merge(e.Access, acc)
	if (e.Kind == KindMissing || e.Kind == KindOther) && kind != KindMissing && kind != KindOther {
		e.Kind = kind
		e.Meta = meta
		e.LinkTarget = linkTarget
	}
	if kind == KindSymlink && e.LinkTarget == "" {
		e.LinkTarget = linkTarget
	}
	return e
}

func (b *Builder) putEntry(e *Entry) *Entry {
	b.entries[e.Path] = e
	return e
}

// attachWarning appends a non-fatal condition to a specific entry.
func (b *Builder) attachWarning(p, msg string) {
	if e, ok := b.entries[p]; ok {
		e.Warnings = append(e.Warnings, msg)
	}
}

// warnf emits a warning through the logger and retains it for test/audit assertions.
func (b *Builder) warnf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	b.logger.Warn(msg)
	b.stats.Warnings = append(b.stats.Warnings, msg)
}

// Build returns an independent snapshot: copied entries, a deterministic lexical
// ordering, and a copy of the counters. The builder remains usable afterwards.
func (b *Builder) Build() *Result {
	out := make(map[string]*Entry, len(b.entries))
	sorted := make([]string, 0, len(b.entries))
	for k, e := range b.entries {
		sorted = append(sorted, k)
		cp := *e
		if len(e.Warnings) > 0 {
			cp.Warnings = append([]string(nil), e.Warnings...)
		}
		out[k] = &cp
	}
	sort.Strings(sorted)
	return &Result{Entries: out, Sorted: sorted, Stats: b.snapshotStats()}
}

func (b *Builder) snapshotStats() Stats {
	s := b.stats
	if len(b.stats.Warnings) > 0 {
		s.Warnings = append([]string(nil), b.stats.Warnings...)
	}
	return s
}

// Reset clears all accumulated state while preserving the root and options.
func (b *Builder) Reset() {
	b.entries = map[string]*Entry{}
	b.done = map[string]struct{}{}
	b.filtered = map[string]struct{}{}
	b.terminal = map[string]string{}
	b.stats = Stats{}
}
