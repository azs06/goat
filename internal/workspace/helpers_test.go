package workspace

// findFiles and grepFiles are the defaults most tests want: ignored files skipped,
// plain-text search.
func findFiles(rootPath, pattern string, limit int) ([]string, bool, error) {
	return FindFiles(rootPath, pattern, limit, false)
}

func grepFiles(rootPath, query string, caseSensitive bool, limit int) ([]string, bool, error) {
	return GrepFiles(rootPath, GrepOptions{Query: query, CaseSensitive: caseSensitive, Limit: limit})
}
