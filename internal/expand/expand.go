// Package expand turns a glob and a command template into a task list: the
// "one agent invocation per matched file" fan-out.
package expand

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// ErrPatternInvalid reports a malformed glob or exclude pattern.
var ErrPatternInvalid = errors.New("expand: invalid pattern")

// DefaultExcludes are always skipped unless the caller overrides them;
// a bare "**/*.js" over a JS project must not fan out into node_modules.
var DefaultExcludes = []string{"**/node_modules/**", "**/.git/**"}

// Files expands the glob patterns relative to root and returns matching
// regular files as slash-separated relative paths, sorted, deduplicated
// case-insensitively on Windows (the filesystem is case-insensitive but
// distinct patterns can match the same file under different casing).
//
// If a pattern is malformed, Files returns an error wrapping
// ErrPatternInvalid. A pattern that matches nothing is not an error.
func Files(root string, patterns, excludes []string) ([]string, error) {
	fsys := os.DirFS(root)
	seen := make(map[string]string, len(patterns))
	for _, pat := range patterns {
		matches, err := doublestar.Glob(fsys, pat, doublestar.WithFilesOnly())
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w: %v", pat, ErrPatternInvalid, err)
		}
		for _, m := range matches {
			skip, err := excluded(m, excludes)
			if err != nil {
				return nil, fmt.Errorf("exclude check for %q: %w", m, err)
			}
			if skip {
				continue
			}
			key := m
			if runtime.GOOS == "windows" {
				key = strings.ToLower(m)
			}
			if _, ok := seen[key]; !ok { // if NOT already seen
				seen[key] = m
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// excluded reports whether path matches any of the exclude patterns. A
// malformed exclude pattern is reported with ErrPatternInvalid.
func excluded(path string, excludes []string) (bool, error) {
	for _, ex := range excludes {
		ok, err := doublestar.Match(ex, path)
		if err != nil {
			return false, fmt.Errorf("exclude pattern %q: %w: %v", ex, ErrPatternInvalid, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// Render substitutes file placeholders into each element of the command
// template. relPath is slash-separated relative to root (as returned by
// Files). Placeholders:
//
//	{path}      absolute path, native separators
//	{slashpath} absolute path, forward slashes (safe in JSON and prompts
//	            on Windows, where backslashes get eaten by escaping)
//	{relpath}   relative path, forward slashes
//	{name}      file name with extension
//	{base}      file name without extension
//	{dir}       absolute directory of the file, native separators
func Render(template []string, root, relPath string) []string {
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	name := filepath.Base(abs)
	base := strings.TrimSuffix(name, filepath.Ext(name))
	repl := strings.NewReplacer(
		"{path}", abs,
		"{slashpath}", filepath.ToSlash(abs),
		"{relpath}", relPath,
		"{name}", name,
		"{base}", base,
		"{dir}", filepath.Dir(abs),
	)
	out := make([]string, len(template))
	for i, arg := range template {
		out[i] = repl.Replace(arg)
	}
	return out
}
