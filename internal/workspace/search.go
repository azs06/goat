package workspace

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// ignoreFilter reports workspace paths that .gitignore excludes, so searches skip build
// output and dependencies. In a git repository git itself decides; elsewhere the root
// .gitignore is read with the common subset of its syntax.
type ignoreFilter struct {
	// files and dirs are what git lists as tracked or not ignored, with forward slashes
	// as git and workspaceRelativePath write them; nil outside git.
	files, dirs map[string]bool
	patterns    []ignorePattern
}

type ignorePattern struct {
	glob     string
	anchored bool
	dirOnly  bool
}

// ignoreFilterOverride, when set, replaces the filter; tests use it.
var ignoreFilterOverride *ignoreFilter

func loadIgnoreFilter() *ignoreFilter {
	if ignoreFilterOverride != nil {
		return ignoreFilterOverride
	}
	filter := &ignoreFilter{}
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Env = CommandEnvironment(os.Environ())
	if out, err := cmd.Output(); err == nil {
		filter.files, filter.dirs = map[string]bool{}, map[string]bool{".": true}
		for _, entry := range bytes.Split(out, []byte{0}) {
			if len(entry) == 0 {
				continue
			}
			rel := string(entry)
			filter.files[rel] = true
			for dir := path.Dir(rel); dir != "." && !filter.dirs[dir]; dir = path.Dir(dir) {
				filter.dirs[dir] = true
			}
		}
		return filter
	}

	data, err := os.ReadFile(".gitignore")
	if err != nil {
		return filter
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		pattern := ignorePattern{}
		if strings.HasSuffix(line, "/") {
			pattern.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.Contains(line, "/") {
			pattern.anchored = true
			line = strings.TrimPrefix(line, "/")
		}
		pattern.glob = line
		filter.patterns = append(filter.patterns, pattern)
	}
	return filter
}

// ignored reports whether the workspace-relative path is excluded.
func (f *ignoreFilter) ignored(rel string, isDir bool) bool {
	if f == nil || rel == "." {
		return false
	}
	slashed := filepath.ToSlash(rel)
	if f.files != nil {
		if isDir {
			return !f.dirs[slashed]
		}
		return !f.files[slashed]
	}
	for _, pattern := range f.patterns {
		if pattern.dirOnly && !isDir {
			continue
		}
		if pattern.anchored {
			if ok, _ := filepath.Match(pattern.glob, slashed); ok {
				return true
			}
			continue
		}
		if ok, _ := filepath.Match(pattern.glob, filepath.Base(rel)); ok {
			return true
		}
	}
	return false
}

// lineMatcher builds the test grep_files applies to each line.
func lineMatcher(query string, regex, caseSensitive bool) (func(string) bool, error) {
	if regex {
		if !caseSensitive {
			query = "(?i)" + query
		}
		re, err := regexp.Compile(query)
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression: %w", err)
		}
		return re.MatchString, nil
	}
	if caseSensitive {
		return func(line string) bool { return strings.Contains(line, query) }, nil
	}
	lowered := strings.ToLower(query)
	return func(line string) bool { return strings.Contains(strings.ToLower(line), lowered) }, nil
}

// matchesGlob reports whether a file passes grep_files' glob: a pattern with a slash is
// matched against the workspace path, one without against the file name.
func matchesGlob(glob, rel string) bool {
	if glob == "" {
		return true
	}
	target := filepath.Base(rel)
	if strings.Contains(glob, "/") {
		target = filepath.ToSlash(rel)
	}
	ok, _ := filepath.Match(glob, target)
	return ok
}

// ReadFileLines returns lines offset through offset+limit-1 (1-based) of a file of any
// size, and how many lines the file has. limit 0 reads to the end.
func ReadFileLines(filePath string, offset, limit int) (text string, first, last, total int, err error) {
	resolvedPath, _, err := ResolvePath(filePath)
	if err != nil {
		return "", 0, 0, 0, err
	}
	file, err := os.Open(resolvedPath)
	if err != nil {
		return "", 0, 0, 0, err
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.IsDir() {
		return "", 0, 0, 0, fmt.Errorf("%s is a directory; use read_dir or tree_dir", filepath.Clean(filePath))
	}

	offset = max(offset, 1)
	var b strings.Builder
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			total++
			if total >= offset && (limit <= 0 || total < offset+limit) {
				if first == 0 {
					first = total
				}
				last = total
				b.WriteString(line)
			}
		}
		if readErr != nil {
			break
		}
	}
	if first == 0 && total > 0 {
		return "", 0, 0, total, fmt.Errorf("%s has %d line(s); offset %d is past the end", filepath.Clean(filePath), total, offset)
	}
	return strings.TrimSuffix(b.String(), "\n"), first, last, total, nil
}
