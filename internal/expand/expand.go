// Package expand turns a glob + command template into a task list —
// the "one agent invocation per matched file" fan-out.
package expand

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// DefaultExcludes are always skipped unless the caller overrides them;
// a bare "**/*.js" over a JS project must not fan out into node_modules.
var DefaultExcludes = []string{"**/node_modules/**", "**/.git/**"}

// Files expands the glob patterns relative to root and returns matching
// regular files as slash-separated relative paths, sorted, deduplicated
// case-insensitively on Windows (the filesystem is case-insensitive but
// distinct patterns can match the same file under different casing).
func Files(root string, patterns, excludes []string) ([]string, error) {
	fsys := os.DirFS(root)
	seen := map[string]string{}
	for _, pat := range patterns {
		matches, err := doublestar.Glob(fsys, pat, doublestar.WithFilesOnly())
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", pat, err)
		}
		for _, m := range matches {
			if excluded(m, excludes) {
				continue
			}
			key := m
			if runtime.GOOS == "windows" {
				key = strings.ToLower(m)
			}
			if _, ok := seen[key]; !ok {
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

// excluded reports whether path matches any of the exclude patterns.
func excluded(path string, excludes []string) bool {
	for _, ex := range excludes {
		if ok, _ := doublestar.Match(ex, path); ok {
			return true
		}
	}
	return false
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
