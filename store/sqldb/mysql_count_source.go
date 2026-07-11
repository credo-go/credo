package sqldb

import (
	"fmt"
	"strings"
)

// validateMySQLCountSource fails before execution when MySQL's derived-table
// output-name rule cannot be proven. PostgreSQL and SQLite allow duplicate
// output labels here, but MySQL requires every derived-table column name to be
// unique. query is the exact single render embedded by the outer count.
func validateMySQLCountSource(query string) error {
	// The server session can enable NO_BACKSLASH_ESCAPES independently of the
	// client. Prove the projection safe under both quote grammars so a raw
	// backslash-quote cannot hide a comma or executable comment from the guard.
	for _, backslashEscapes := range []bool{true, false} {
		if err := validateMySQLCountSourceMode(query, backslashEscapes); err != nil {
			return err
		}
	}
	return nil
}

func validateMySQLCountSourceMode(query string, backslashEscapes bool) error {
	expressions, err := mysqlMainSelectExpressionsMode(query, backslashEscapes)
	if err != nil {
		//nolint:errorlint // Parser detail is diagnostic; the sentinel is the sole wrapped contract.
		return fmt.Errorf("%w: MySQL count source projection: %v", ErrUnsupportedCountQuery, err)
	}

	seen := make(map[string]int, len(expressions))
	for i, expression := range expressions {
		if i == 0 {
			expression = stripMySQLSelectModifiers(expression)
		}
		name, err := mysqlProjectionOutputNameMode(expression, backslashEscapes)
		if err != nil {
			return fmt.Errorf(
				"%w: MySQL count source projection %d: %v; "+
					"give every raw expression an explicit unique AS alias",
				ErrUnsupportedCountQuery,
				i+1,
				err,
			)
		}
		key := strings.ToLower(name)
		if previous, ok := seen[key]; ok {
			return fmt.Errorf(
				"%w: MySQL derived count source has duplicate output name %q "+
					"at projections %d and %d; alias projected columns uniquely",
				ErrUnsupportedCountQuery,
				name,
				previous,
				i+1,
			)
		}
		seen[key] = i + 1
	}
	return nil
}

func mysqlMainSelectExpressions(query string) ([]string, error) {
	return mysqlMainSelectExpressionsMode(query, true)
}

func mysqlMainSelectExpressionsMode(query string, backslashEscapes bool) ([]string, error) {
	if hasMySQLExecutableComment(query, backslashEscapes) {
		return nil, fmt.Errorf("executable or optimizer comments are not supported")
	}
	selectStart := -1
	selectEnd := -1
	depth := 0
	forEachMySQLSQLToken(query, backslashEscapes, func(token string, start, end, tokenDepth int) bool {
		if tokenDepth == 0 && strings.EqualFold(token, "SELECT") {
			selectStart = start
			selectEnd = end
			return false
		}
		return true
	}, &depth)
	if selectStart < 0 {
		return nil, fmt.Errorf("main SELECT was not found")
	}

	listEnd := len(query)
	depth = 0
	forEachMySQLSQLToken(query[selectEnd:], backslashEscapes, func(token string, start, _, tokenDepth int) bool {
		if tokenDepth != 0 {
			return true
		}
		switch strings.ToUpper(token) {
		case "FROM", "WHERE", "GROUP", "HAVING", "WINDOW", "ORDER", "LIMIT", "FOR",
			"LOCK", "INTO", "UNION", "INTERSECT", "EXCEPT":
			listEnd = selectEnd + start
			return false
		default:
			return true
		}
	}, &depth)
	if depth != 0 {
		return nil, fmt.Errorf("SELECT list has unbalanced parentheses")
	}

	list := query[selectEnd:listEnd]
	expressions := splitMySQLTopLevel(list, ',', backslashEscapes)
	if len(expressions) == 0 {
		return nil, fmt.Errorf("SELECT list is empty")
	}
	for _, expression := range expressions {
		if strings.TrimSpace(expression) == "" {
			return nil, fmt.Errorf("SELECT list contains an empty expression")
		}
	}
	return expressions, nil
}

// forEachMySQLSQLToken visits bare SQL tokens outside quotes and comments. The
// callback receives the parenthesis depth at the token's first byte and stops
// iteration by returning false. finalDepth is reset by the caller and updated
// so the same scanner rules are shared by SELECT discovery and list parsing.
func forEachMySQLSQLToken(
	query string,
	backslashEscapes bool,
	visit func(token string, start, end, depth int) bool,
	finalDepth *int,
) {
	depth := *finalDepth
	for i := 0; i < len(query); {
		switch query[i] {
		case '\'', '"', '`':
			i = skipMySQLQuoted(query, i, query[i], backslashEscapes)
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				i = skipMySQLBlockComment(query, i+2)
			} else {
				i++
			}
		case '-':
			if i+2 < len(query) && query[i+1] == '-' && isMySQLDashCommentFollower(query[i+2]) {
				i = skipMySQLLineComment(query, i+3)
			} else {
				i++
			}
		case '#':
			i = skipMySQLLineComment(query, i+1)
		case '(':
			depth++
			i++
		case ')':
			if depth > 0 {
				depth--
			}
			i++
		default:
			if isMySQLTokenByte(query[i]) {
				start := i
				for i < len(query) && isMySQLTokenByte(query[i]) {
					i++
				}
				if !visit(query[start:i], start, i, depth) {
					*finalDepth = depth
					return
				}
			} else {
				i++
			}
		}
	}
	*finalDepth = depth
}

func splitMySQLTopLevel(query string, separator byte, backslashEscapes bool) []string {
	var expressions []string
	depth := 0
	start := 0
	for i := 0; i < len(query); {
		switch query[i] {
		case '\'', '"', '`':
			i = skipMySQLQuoted(query, i, query[i], backslashEscapes)
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				i = skipMySQLBlockComment(query, i+2)
			} else {
				i++
			}
		case '-':
			if i+2 < len(query) && query[i+1] == '-' && isMySQLDashCommentFollower(query[i+2]) {
				i = skipMySQLLineComment(query, i+3)
			} else {
				i++
			}
		case '#':
			i = skipMySQLLineComment(query, i+1)
		case '(':
			depth++
			i++
		case ')':
			if depth > 0 {
				depth--
			}
			i++
		default:
			if query[i] == separator && depth == 0 {
				expressions = append(expressions, strings.TrimSpace(query[start:i]))
				start = i + 1
			}
			i++
		}
	}
	expressions = append(expressions, strings.TrimSpace(query[start:]))
	return expressions
}

func hasMySQLExecutableComment(query string, backslashEscapes bool) bool {
	for i := 0; i < len(query); {
		switch query[i] {
		case '\'', '"', '`':
			i = skipMySQLQuoted(query, i, query[i], backslashEscapes)
		case '/':
			if i+1 >= len(query) || query[i+1] != '*' {
				i++
				continue
			}
			if i+2 < len(query) && (query[i+2] == '!' || query[i+2] == '+') {
				return true
			}
			if i+3 < len(query) && (query[i+2] == 'M' || query[i+2] == 'm') &&
				query[i+3] == '!' {
				return true
			}
			i = skipMySQLBlockComment(query, i+2)
		case '-':
			if i+2 < len(query) && query[i+1] == '-' && isMySQLDashCommentFollower(query[i+2]) {
				i = skipMySQLLineComment(query, i+3)
			} else {
				i++
			}
		case '#':
			i = skipMySQLLineComment(query, i+1)
		default:
			i++
		}
	}
	return false
}

func isMySQLDashCommentFollower(b byte) bool {
	return b <= ' ' || b == 0x7f
}

func mysqlProjectionOutputName(expression string) (string, error) {
	return mysqlProjectionOutputNameMode(expression, true)
}

func mysqlProjectionOutputNameMode(expression string, backslashEscapes bool) (string, error) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return "", fmt.Errorf("empty expression")
	}

	if asStart, asEnd, ok := lastMySQLTopLevelKeyword(expression, "AS", backslashEscapes); ok {
		if strings.TrimSpace(expression[:asStart]) == "" {
			return "", fmt.Errorf("AS alias has no expression")
		}
		alias, ok := parseMySQLSingleIdentifier(expression[asEnd:])
		if !ok {
			return "", fmt.Errorf("AS alias is not one SQL identifier")
		}
		if !isProvenMySQLOutputName(alias) {
			return "", fmt.Errorf("AS alias is outside the portable ASCII identifier subset")
		}
		return alias, nil
	}

	parts, wildcard, ok := parseMySQLQualifiedIdentifier(expression)
	if wildcard {
		return "", fmt.Errorf("wildcard output width and names are not provable")
	}
	if !ok || len(parts) == 0 {
		return "", fmt.Errorf("output name is not provable without AS")
	}
	name := parts[len(parts)-1]
	if !isProvenMySQLOutputName(name) {
		return "", fmt.Errorf("output name is outside the portable ASCII identifier subset")
	}
	return name, nil
}

func lastMySQLTopLevelKeyword(query, keyword string, backslashEscapes bool) (int, int, bool) {
	start := -1
	end := -1
	depth := 0
	forEachMySQLSQLToken(query, backslashEscapes, func(token string, tokenStart, tokenEnd, tokenDepth int) bool {
		if tokenDepth == 0 && strings.EqualFold(token, keyword) {
			start = tokenStart
			end = tokenEnd
		}
		return true
	}, &depth)
	return start, end, start >= 0
}

func parseMySQLQualifiedIdentifier(query string) ([]string, bool, bool) {
	query = strings.TrimSpace(query)
	if query == "*" {
		return nil, true, true
	}

	var parts []string
	for len(query) > 0 {
		query = strings.TrimSpace(query)
		if query == "*" {
			return parts, true, true
		}
		identifier, rest, ok := consumeMySQLIdentifier(query)
		if !ok {
			return nil, false, false
		}
		parts = append(parts, identifier)
		query = strings.TrimSpace(rest)
		if query == "" {
			return parts, false, true
		}
		if query[0] != '.' {
			return nil, false, false
		}
		query = query[1:]
	}
	return nil, false, false
}

func parseMySQLSingleIdentifier(query string) (string, bool) {
	identifier, rest, ok := consumeMySQLIdentifier(strings.TrimSpace(query))
	return identifier, ok && strings.TrimSpace(rest) == ""
}

func consumeMySQLIdentifier(query string) (string, string, bool) {
	if query == "" {
		return "", query, false
	}
	if query[0] == '`' {
		quote := query[0]
		var b strings.Builder
		for i := 1; i < len(query); i++ {
			if query[i] == '\\' {
				// Backslash handling for quoted identifiers is session-mode
				// sensitive. It is outside the portable identifier subset used
				// by this proof, so reject it instead of guessing.
				return "", query, false
			}
			if query[i] == quote {
				if i+1 < len(query) && query[i+1] == quote {
					b.WriteByte(quote)
					i++
					continue
				}
				return b.String(), query[i+1:], b.Len() > 0
			}
			b.WriteByte(query[i])
		}
		return "", query, false
	}

	i := 0
	for i < len(query) && isMySQLIdentifierByte(query[i]) {
		i++
	}
	if i == 0 {
		return "", query, false
	}
	return query[:i], query[i:], true
}

func stripMySQLSelectModifiers(expression string) string {
	expression = strings.TrimSpace(expression)
	for {
		token, rest := firstMySQLBareToken(expression)
		if !isMySQLSelectModifier(strings.ToUpper(token)) {
			return expression
		}
		expression = strings.TrimSpace(rest)
	}
}

func isMySQLSelectModifier(token string) bool {
	switch token {
	case "ALL", "DISTINCT", "DISTINCTROW", "HIGH_PRIORITY", "STRAIGHT_JOIN",
		"SQL_SMALL_RESULT", "SQL_BIG_RESULT", "SQL_BUFFER_RESULT", "SQL_NO_CACHE",
		"SQL_CALC_FOUND_ROWS":
		return true
	default:
		return false
	}
}

func firstMySQLBareToken(query string) (string, string) {
	i := 0
	for i < len(query) && isMySQLTokenByte(query[i]) {
		i++
	}
	return query[:i], query[i:]
}

func skipMySQLQuoted(query string, start int, quote byte, backslashEscapes bool) int {
	for i := start + 1; i < len(query); i++ {
		if backslashEscapes && query[i] == '\\' && i+1 < len(query) {
			i++
			continue
		}
		if query[i] != quote {
			continue
		}
		if i+1 < len(query) && query[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(query)
}

func skipMySQLBlockComment(query string, start int) int {
	if end := strings.Index(query[start:], "*/"); end >= 0 {
		return start + end + 2
	}
	return len(query)
}

func skipMySQLLineComment(query string, start int) int {
	if end := strings.IndexByte(query[start:], '\n'); end >= 0 {
		return start + end + 1
	}
	return len(query)
}

func isMySQLTokenByte(b byte) bool {
	return isMySQLIdentifierByte(b) || b >= 0x80
}

func isMySQLIdentifierByte(b byte) bool {
	return b == '_' || b == '$' || b >= '0' && b <= '9' ||
		b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func isProvenMySQLOutputName(name string) bool {
	if name == "" {
		return false
	}
	for i := range len(name) {
		if !isMySQLIdentifierByte(name[i]) {
			return false
		}
	}
	return true
}
