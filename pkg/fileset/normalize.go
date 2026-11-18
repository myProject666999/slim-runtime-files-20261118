package fileset

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// normalize canonicalizes a container path. Container paths are always absolute;
// a relative or empty input is rejected so the caller never silently treats a
// container-relative name as host-relative. filepath.Clean collapses redundant
// separators, "." and lexical "..", clamping any ".." that would cross the root
// (e.g. "/a/../../etc" becomes "/etc").
func normalize(containerPath string) (string, bool) {
	if containerPath == "" || !filepath.IsAbs(containerPath) {
		return "", false
	}
	cleaned := filepath.Clean(containerPath)
	if cleaned == "." || !filepath.IsAbs(cleaned) {
		return "", false
	}
	return cleaned, true
}

// hostOf maps a normalized container path onto the extracted root on the host and
// enforces a containment fence. Even though normalize() already lexicalizes "..",
// a symlink target string is joined at runtime, so this is a defense-in-depth guard
// ensuring a target can never point the Lstat/Readlink calls outside rootDir.
// The boolean is false when the resolved host path escapes rootDir.
func (b *Builder) hostOf(containerPath string) (string, bool) {
	host := filepath.Join(b.rootDir, containerPath)
	rel, err := filepath.Rel(b.rootDir, host)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return host, true
}

// resolveTarget re-anchors a raw symlink target into the container namespace.
//
// An absolute target is resolved against the CONTAINER root "/", never the host
// root: this is the deliberate fix over dockerimage.linkTargetToFullPath, which
// returns absolute targets verbatim and thereby leaks to the host filesystem.
// A relative target is resolved against the directory that contains the link.
// Both inputs and the result are normalized container paths.
func resolveTarget(linkContainerPath, target string) string {
	if path.IsAbs(target) {
		return path.Join("/", target)
	}
	return path.Join(path.Dir(linkContainerPath), target)
}

// splitComponents breaks a cleaned absolute container path into its non-root
// components. splitComponents("/") returns nil; splitComponents("/a/b") returns
// ["a","b"]. The slash-only path package is used because container paths use "/"
// regardless of the host OS.
func splitComponents(cleanAbsPath string) []string {
	trimmed := strings.TrimPrefix(cleanAbsPath, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// rank returns the strength of an Access; larger is stronger.
func rank(a Access) int { return int(a) }

// merge keeps the stronger of two observed access levels. It makes read-then-write
// on the same path converge on AccessWrite regardless of event order.
func merge(existing, incoming Access) Access {
	if rank(incoming) > rank(existing) {
		return incoming
	}
	return existing
}
