package sshconfig

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

const (
	maxIncludeDepth = 32
	maxConfigLine   = 1 << 20
)

type directive struct {
	keyword   string
	arguments []string
	source    string
	line      int
}

type configFile struct {
	path       string
	canonical  string
	directives []directive
}

type document struct {
	root   configFile
	loader *documentLoader
}

func loadDocument(ctx context.Context, source string, account systemAccount) (document, bool, error) {
	loader := &documentLoader{
		account:     account,
		includeBase: filepath.Join(account.homeDir, ".ssh"),
		cache:       make(map[string]configFile),
	}
	root, available, err := loader.loadFile(ctx, source, true)
	if err != nil {
		return document{}, false, err
	}
	if !available {
		return document{}, false, nil
	}
	return document{root: root, loader: loader}, true, nil
}

type documentLoader struct {
	account     systemAccount
	includeBase string
	cache       map[string]configFile
}

func (l *documentLoader) loadFile(ctx context.Context, path string, root bool) (configFile, bool, error) {
	// Reading files is the only part of discovery that can block, so this is
	// the one cancellation check below the two API entry points.
	if err := ctx.Err(); err != nil {
		return configFile{}, false, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return configFile{}, false, fmt.Errorf("sshconfig: resolve config path %s: %w", path, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return configFile{}, false, nil
		}
		return configFile{}, false, fmt.Errorf("sshconfig: stat %s: %w", absolute, err)
	}
	if !info.Mode().IsRegular() {
		return configFile{}, false, fmt.Errorf("sshconfig: config %s is not a regular file", absolute)
	}
	canonical := absolute
	if evaluated, evalErr := filepath.EvalSymlinks(absolute); evalErr == nil {
		canonical = filepath.Clean(evaluated)
	}
	if cached, ok := l.cache[canonical]; ok {
		return cached, true, nil
	}

	file, err := os.Open(absolute)
	if err != nil {
		if !root && errors.Is(err, fs.ErrNotExist) {
			return configFile{}, false, nil
		}
		return configFile{}, false, fmt.Errorf("sshconfig: open %s: %w", absolute, err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return configFile{}, false, fmt.Errorf("sshconfig: stat opened %s: %w", absolute, err)
	}
	if !openedInfo.Mode().IsRegular() {
		return configFile{}, false, fmt.Errorf("sshconfig: config %s is not a regular file", absolute)
	}

	parsed := configFile{path: absolute, canonical: canonical}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), maxConfigLine)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		keyword, arguments, ok, err := parseDirective(scanner.Text())
		if err != nil {
			return configFile{}, false, fmt.Errorf("sshconfig: %s:%d: %w", absolute, lineNumber, err)
		}
		if !ok {
			continue
		}
		parsed.directives = append(parsed.directives, directive{
			keyword: keyword, arguments: arguments, source: absolute, line: lineNumber,
		})
	}
	if err := scanner.Err(); err != nil {
		return configFile{}, false, fmt.Errorf("sshconfig: read %s: %w", absolute, err)
	}
	l.cache[canonical] = parsed
	return parsed, true, nil
}

func (d document) includedFiles(ctx context.Context, include directive) ([]configFile, error) {
	files := make([]configFile, 0)
	for _, pattern := range include.arguments {
		matches, err := includeMatches(pattern, d.loader.includeBase, d.loader.account)
		if err != nil {
			return nil, fmt.Errorf("sshconfig: %s:%d: Include %q: %w", include.source, include.line, pattern, err)
		}
		for _, match := range matches {
			file, available, err := d.loader.loadFile(ctx, match, false)
			if err != nil {
				return nil, err
			}
			if available {
				files = append(files, file)
			}
		}
	}
	return files, nil
}

func includeMatches(pattern, relativeTo string, account systemAccount) ([]string, error) {
	expanded, err := expandEnvironment(pattern)
	if err != nil {
		return nil, err
	}
	expanded, err = expandIncludeTokens(expanded, account)
	if err != nil {
		return nil, err
	}
	expanded, err = expandHome(expanded, account.homeDir)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(relativeTo, expanded)
	}
	matches, err := filepath.Glob(filepath.Clean(expanded))
	if err != nil {
		return nil, err
	}
	slices.Sort(matches)
	return matches, nil
}

func expandEnvironment(value string) (string, error) {
	var expanded strings.Builder
	for index := 0; index < len(value); {
		if value[index] != '$' || index+1 >= len(value) || value[index+1] != '{' {
			expanded.WriteByte(value[index])
			index++
			continue
		}
		end := strings.IndexByte(value[index+2:], '}')
		if end < 0 {
			return "", errors.New("unterminated environment variable")
		}
		end += index + 2
		name := value[index+2 : end]
		if !validEnvironmentName(name) {
			return "", fmt.Errorf("invalid environment variable %q", name)
		}
		replacement, exists := os.LookupEnv(name)
		if !exists {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		expanded.WriteString(replacement)
		index = end + 1
	}
	return expanded.String(), nil
}

func validEnvironmentName(name string) bool {
	if name == "" || !isEnvironmentNameStart(name[0]) {
		return false
	}
	for index := 1; index < len(name); index++ {
		character := name[index]
		if !isEnvironmentNameStart(character) && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func isEnvironmentNameStart(character byte) bool {
	return character == '_' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

func expandIncludeTokens(value string, account systemAccount) (string, error) {
	return expandPercentTokens(value, func(token byte) (string, error) {
		switch token {
		case 'd':
			return account.homeDir, nil
		case 'u':
			return account.username, nil
		case 'i':
			if account.uid == "" {
				return "", errors.New("resolve %i user ID: empty user ID")
			}
			return account.uid, nil
		case 'l', 'L':
			hostname, err := os.Hostname()
			if err != nil {
				return "", fmt.Errorf("resolve %%%c local hostname: %w", token, err)
			}
			if token == 'L' {
				hostname, _, _ = strings.Cut(hostname, ".")
			}
			return hostname, nil
		case 'C', 'h', 'j', 'k', 'n', 'p', 'r':
			return "", fmt.Errorf("token %%%c requires a target host and is unsafe during discovery", token)
		default:
			return "", fmt.Errorf("unsupported token %%%c", token)
		}
	})
}

func expandPercentTokens(value string, resolve func(byte) (string, error)) (string, error) {
	var expanded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '%' {
			expanded.WriteByte(value[index])
			continue
		}
		if index+1 >= len(value) {
			return "", errors.New("unfinished percent token")
		}
		index++
		token := value[index]
		if token == '%' {
			expanded.WriteByte('%')
			continue
		}
		replacement, err := resolve(token)
		if err != nil {
			return "", err
		}
		expanded.WriteString(replacement)
	}
	return expanded.String(), nil
}

// aliasCollector accumulates importable Host aliases in first-seen order and
// answers membership without a second pass.
type aliasCollector struct {
	order []string
	seen  map[string]struct{}
}

func (c *aliasCollector) add(alias string) {
	if _, exists := c.seen[alias]; exists {
		return
	}
	c.seen[alias] = struct{}{}
	c.order = append(c.order, alias)
}

func (d document) literalAliases(ctx context.Context) (aliasCollector, error) {
	collector := aliasCollector{order: make([]string, 0), seen: make(map[string]struct{})}
	stack := map[string]bool{d.root.canonical: true}
	if err := d.collectAliases(ctx, d.root, &collector, stack, 0); err != nil {
		return aliasCollector{}, err
	}
	return collector, nil
}

func (d document) collectAliases(ctx context.Context, file configFile, collector *aliasCollector, stack map[string]bool, depth int) error {
	if depth > maxIncludeDepth {
		return fmt.Errorf("sshconfig: include depth exceeds %d at %s", maxIncludeDepth, file.path)
	}
	includeAllowed := true
	for _, directive := range file.directives {
		switch directive.keyword {
		case "host":
			for _, pattern := range directive.arguments {
				if pattern == "" || strings.HasPrefix(pattern, "!") || strings.ContainsAny(pattern, "*?") {
					continue
				}
				collector.add(pattern)
			}
			// An unnegated literal "*" matches every possible target, so an
			// Include in that block is as safe to enumerate as a global Include.
			// Every other Host condition remains fail-closed during discovery.
			includeAllowed = hostPatternsAreUniversal(directive.arguments)
		case "match":
			includeAllowed = isMatchAll(directive.arguments)
		case "include":
			if !includeAllowed {
				continue
			}
			if depth >= maxIncludeDepth {
				return fmt.Errorf("sshconfig: include depth exceeds %d at %s:%d", maxIncludeDepth, directive.source, directive.line)
			}
			included, err := d.includedFiles(ctx, directive)
			if err != nil {
				return err
			}
			for _, child := range included {
				if stack[child.canonical] {
					return fmt.Errorf("sshconfig: recursive Include cycle at %s", child.path)
				}
				stack[child.canonical] = true
				err := d.collectAliases(ctx, child, collector, stack, depth+1)
				delete(stack, child.canonical)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func isMatchAll(arguments []string) bool {
	return len(arguments) == 1 && strings.EqualFold(arguments[0], "all")
}

// hostPatternsAreUniversal reports whether the pattern list matches every
// possible target, i.e. contains an unnegated "*".
func hostPatternsAreUniversal(patterns []string) bool {
	hasUniversalPattern := false
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, "!") {
			return false
		}
		if pattern == "*" {
			hasUniversalPattern = true
		}
	}
	return hasUniversalPattern
}

func parseDirective(line string) (keyword string, arguments []string, ok bool, err error) {
	line = strings.TrimSpace(stripComment(line))
	if line == "" {
		return "", nil, false, nil
	}
	keywordEnd := strings.IndexFunc(line, func(r rune) bool {
		return unicode.IsSpace(r) || r == '='
	})
	var remainder string
	if keywordEnd < 0 {
		keyword, remainder = line, ""
	} else {
		keyword, remainder = line[:keywordEnd], line[keywordEnd:]
	}
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	if keyword == "" {
		return "", nil, false, errors.New("missing keyword")
	}
	remainder = strings.TrimSpace(remainder)
	if strings.HasPrefix(remainder, "=") {
		remainder = strings.TrimSpace(remainder[1:])
	}
	arguments, err = splitArguments(remainder)
	if err != nil {
		return "", nil, false, err
	}
	return keyword, arguments, true, nil
}

func stripComment(line string) string {
	var quote rune
	escaped := false
	for index, value := range line {
		if escaped {
			escaped = false
			continue
		}
		if value == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if value == quote {
				quote = 0
			}
			continue
		}
		if value == '"' {
			quote = value
			continue
		}
		if value == '#' {
			return line[:index]
		}
	}
	return line
}

func splitArguments(value string) ([]string, error) {
	arguments := make([]string, 0)
	var current strings.Builder
	var quote rune
	escaped := false
	started := false
	flush := func() {
		if started {
			arguments = append(arguments, current.String())
			current.Reset()
			started = false
		}
	}
	for _, character := range value {
		if escaped {
			current.WriteRune(character)
			started = true
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				current.WriteRune(character)
			}
			started = true
			continue
		}
		if character == '"' {
			quote = character
			started = true
			continue
		}
		if unicode.IsSpace(character) {
			flush()
			continue
		}
		current.WriteRune(character)
		started = true
	}
	if escaped {
		return nil, errors.New("unfinished escape")
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	flush()
	return arguments, nil
}

func hostPatternsMatch(patterns []string, alias string) bool {
	matched := false
	for _, candidate := range patterns {
		negated := strings.HasPrefix(candidate, "!")
		pattern := strings.TrimPrefix(candidate, "!")
		if pattern == "" || !wildcardMatch(pattern, alias) {
			continue
		}
		if negated {
			return false
		}
		matched = true
	}
	return matched
}

// wildcardMatch deliberately does not use path.Match: OpenSSH Host patterns
// support only "*" and "?", so the "[...]" character classes path.Match would
// interpret must stay literal, and path.Match also treats "/" specially.
func wildcardMatch(pattern, value string) bool {
	patternRunes := []rune(pattern)
	valueRunes := []rune(value)
	patternIndex, valueIndex := 0, 0
	lastStar, valueAfterStar := -1, 0
	for valueIndex < len(valueRunes) {
		if patternIndex < len(patternRunes) && (patternRunes[patternIndex] == '?' || patternRunes[patternIndex] == valueRunes[valueIndex]) {
			patternIndex++
			valueIndex++
			continue
		}
		if patternIndex < len(patternRunes) && patternRunes[patternIndex] == '*' {
			lastStar = patternIndex
			patternIndex++
			valueAfterStar = valueIndex
			continue
		}
		if lastStar < 0 {
			return false
		}
		patternIndex = lastStar + 1
		valueAfterStar++
		valueIndex = valueAfterStar
	}
	for patternIndex < len(patternRunes) && patternRunes[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(patternRunes)
}
