package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// predicate consumes the flat, typed ECS field map, not raw source bytes.
type predicate func(map[string]any) bool

const (
	detectionMaxSelections = 256
	detectionMaxMapItems = 256
	detectionMaxListItems = 1024
	detectionMaxDepth = 16
	detectionMaxStringBytes = 64 * 1024
	detectionMaxNodes = 16384
	detectionMaxPatternBytes = 1024 * 1024
	conditionMaxBytes = 8192
	conditionMaxTokens = 1024
	conditionMaxDepth = 64
)

// Sigma 2.1 semantics are specified at:
// https://sigmahq.io/sigma-specification/specification/sigma-rules-specification.html
// https://sigmahq.io/sigma-specification/specification/sigma-appendix-modifiers.html
// This backend keeps values typed. A string never aliases a number or boolean.
// Sigma leaves null backend-dependent: here null means PRESENT with a null
// value. Use |exists: false for absence. Nested event maps are never searched;
// an ECS dotted field name is an exact key in the normalized envelope.
func compileDetection(detection map[string]any) (predicate, error) {
	if len(detection) < 2 || len(detection) > detectionMaxSelections+1 {
		return nil, errors.New("detection requires 1 to 256 selections and a condition")
	}
	condition, found := detection["condition"]
	if !found {
		return nil, errors.New("detection condition is required")
	}
	names := make([]string, 0, len(detection)-1)
	for name := range detection {
		if name == "condition" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "timeframe" {
			return nil, errors.New("unsupported detection feature: timeframe; use a Sigma correlation timespan")
		}
		if !conditionIdentifier(name, false) || conditionReserved(name) {
			return nil, fmt.Errorf("invalid selection name: %q", name)
		}
	}
	budget := detectionBudget{}
	selections := make(map[string]predicate, len(names))
	for _, name := range names {
		compiled, err := budget.selection(detection[name], 0)
		if err != nil {
			return nil, fmt.Errorf("selection %s: %w", name, err)
		}
		selections[name] = compiled
	}
	conditions, ok := detectionList(condition)
	if !ok {
		conditions = []any{condition}
	}
	if len(conditions) == 0 || len(conditions) > detectionMaxListItems {
		return nil, errors.New("condition list must contain 1 to 1024 expressions")
	}
	compiled := make([]predicate, 0, len(conditions))
	for _, value := range conditions {
		expression, ok := value.(string)
		if !ok {
			return nil, errors.New("condition must be a string or a list of strings")
		}
		budget.conditionBytes += len(expression)
		if budget.conditionBytes > conditionMaxBytes {
			return nil, errors.New("condition exceeds 8192 total expression bytes")
		}
		tokens, err := conditionLex(expression)
		if err != nil {
			return nil, err
		}
		budget.conditionTokens += len(tokens)
		if budget.conditionTokens > conditionMaxTokens {
			return nil, errors.New("condition exceeds 1024 total tokens")
		}
		parser := conditionParser{tokens: tokens, selections: selections, names: names}
		result, err := parser.parseOr(0)
		if err != nil {
			return nil, err
		}
		if parser.position != len(tokens) {
			return nil, fmt.Errorf("unexpected condition token: %q", tokens[parser.position])
		}
		compiled = append(compiled, result)
	}
	return detectionOr(compiled), nil
}

type detectionBudget struct {
	nodes int
	patternBytes int
	conditionBytes int
	conditionTokens int
}

func (b *detectionBudget) node(depth int) error {
	b.nodes++
	if depth > detectionMaxDepth || b.nodes > detectionMaxNodes {
		return errors.New("detection exceeds structural node/depth limit")
	}
	return nil
}

func (b *detectionBudget) text(value string) error {
	b.patternBytes += len(value)
	if len(value) > detectionMaxStringBytes || b.patternBytes > detectionMaxPatternBytes || !utf8.ValidString(value) {
		return errors.New("detection exceeds string/total pattern byte limit or has invalid UTF-8")
	}
	return nil
}

func (b *detectionBudget) selection(value any, depth int) (predicate, error) {
	if err := b.node(depth); err != nil {
		return nil, err
	}
	if object, ok := value.(map[string]any); ok {
		if len(object) == 0 || len(object) > detectionMaxMapItems {
			return nil, errors.New("selection map must contain 1 to 256 items")
		}
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]predicate, 0, len(keys))
		for _, key := range keys {
			part, err := b.field(key, object[key], depth)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
		return detectionAnd(parts), nil
	}
	list, ok := detectionList(value)
	if !ok || len(list) == 0 || len(list) > detectionMaxListItems {
		return nil, errors.New("selection must be a non-empty bounded map or list")
	}
	parts := make([]predicate, 0, len(list))
	for _, item := range list {
		if keyword, ok := item.(string); ok {
			if err := b.node(depth); err != nil {
				return nil, err
			}
			match, err := b.scalar(keyword, detectionModifiers{mode: "contains"})
			if err != nil {
				return nil, err
			}
			parts = append(parts, func(fields map[string]any) bool { return detectionActual(fields["message"], match) })
			continue
		}
		part, err := b.selection(item, depth+1)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return detectionOr(parts), nil
}

type detectionModifiers struct {
	mode string
	all bool
	cased bool
	regexFlags string
}

func detectionField(key string) (string, detectionModifiers, error) {
	var modifiers detectionModifiers
	if len(key) == 0 || len(key) > 256 || !utf8.ValidString(key) {
		return "", modifiers, errors.New("invalid detection field name")
	}
	parts := strings.Split(key, "|")
	field := parts[0]
	if field == "" && key != "|all" {
		return "", modifiers, errors.New("unsupported keyword modifier combination; only |all is supported")
	}
	for _, r := range field {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.@-", r)) {
			return "", modifiers, errors.New("invalid detection field name")
		}
	}
	seen := make(map[string]bool)
	for _, modifier := range parts[1:] {
		if seen[modifier] {
			return "", modifiers, fmt.Errorf("duplicate modifier: %s", modifier)
		}
		seen[modifier] = true
		switch modifier {
		case "all":
			modifiers.all = true
		case "cased":
			modifiers.cased = true
		case "contains", "startswith", "endswith", "re", "cidr", "exists":
			if modifiers.mode != "" {
				return "", modifiers, fmt.Errorf("unsupported modifier combination: %s with %s", modifiers.mode, modifier)
			}
			modifiers.mode = modifier
		case "i", "m", "s":
			if modifiers.mode != "re" {
				return "", modifiers, fmt.Errorf("unsupported modifier combination: %s requires preceding re", modifier)
			}
			modifiers.regexFlags += modifier
		default:
			return "", modifiers, fmt.Errorf("unsupported modifier: %s; not implemented by the stream Sigma subset", modifier)
		}
	}
	if modifiers.cased && (modifiers.mode == "re" || modifiers.mode == "cidr" || modifiers.mode == "exists") {
		return "", modifiers, fmt.Errorf("unsupported modifier combination: cased with %s", modifiers.mode)
	}
	if modifiers.mode == "exists" && modifiers.all {
		return "", modifiers, errors.New("unsupported modifier combination: exists with all")
	}
	return field, modifiers, nil
}

func (b *detectionBudget) field(key string, value any, depth int) (predicate, error) {
	if err := b.node(depth); err != nil {
		return nil, err
	}
	field, modifiers, err := detectionField(key)
	if err != nil {
		return nil, err
	}
	if modifiers.mode == "exists" {
		exists, ok := value.(bool)
		if !ok {
			return nil, errors.New("modifier exists requires a boolean scalar")
		}
		return func(fields map[string]any) bool { _, found := fields[field]; return found == exists }, nil
	}
	if value == nil {
		if modifiers.mode != "" || modifiers.all || modifiers.cased || field == "" {
			return nil, errors.New("null does not support field modifiers")
		}
		return func(fields map[string]any) bool { actual, found := fields[field]; return found && actual == nil }, nil
	}
	values, isList := detectionList(value)
	if modifiers.all && !isList {
		return nil, errors.New("modifier all requires a list, not a single scalar")
	}
	if !isList {
		values = []any{value}
	}
	if len(values) == 0 || len(values) > detectionMaxListItems {
		return nil, errors.New("field value list must contain 1 to 1024 items")
	}
	if field == "" {
		for _, item := range values {
			if _, ok := item.(string); !ok {
				return nil, errors.New("keyword |all requires string values")
			}
		}
		field = "message"
		modifiers.mode = "contains"
	}
	parts := make([]predicate, 0, len(values))
	for _, item := range values {
		if err := b.node(depth); err != nil {
			return nil, err
		}
		if item == nil {
			return nil, errors.New("null cannot be part of a Sigma field value list")
		}
		match, err := b.scalar(item, modifiers)
		if err != nil {
			return nil, err
		}
		parts = append(parts, func(fields map[string]any) bool {
			actual, found := fields[field]
			return found && detectionActual(actual, match)
		})
	}
	if modifiers.all {
		return detectionAnd(parts), nil
	}
	return detectionOr(parts), nil
}

func (b *detectionBudget) scalar(value any, modifiers detectionModifiers) (func(any) bool, error) {
	if text, ok := value.(string); ok {
		if err := b.text(text); err != nil {
			return nil, err
		}
		if modifiers.mode == "cidr" {
			prefix, err := netip.ParsePrefix(text)
			if err != nil || prefix.Addr().Zone() != "" {
				return nil, errors.New("invalid cidr modifier value: expected IPv4 or IPv6 CIDR prefix")
			}
			prefix = prefix.Masked()
			return func(actual any) bool {
				ip, ok := actual.(string)
				if !ok {
					return false
				}
				address, err := netip.ParseAddr(ip)
				return err == nil && address.Zone() == "" && prefix.Contains(address)
			}, nil
		}
		var pattern string
		if modifiers.mode == "re" {
			if err := detectionRegexSubset(text); err != nil {
				return nil, err
			}
			pattern = text
			if modifiers.regexFlags != "" {
				pattern = "(?" + modifiers.regexFlags + ")" + pattern
			}
		} else {
			pattern = detectionWildcard(text, modifiers.mode, modifiers.cased)
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, errors.New("invalid regular expression: unsupported or malformed RE2/Sigma pattern")
		}
		return func(actual any) bool { text, ok := actual.(string); return ok && compiled.MatchString(text) }, nil
	}
	if modifiers.mode != "" || modifiers.cased {
		return nil, fmt.Errorf("modifier %s requires string values", modifiers.mode)
	}
	if boolean, ok := value.(bool); ok {
		return func(actual any) bool { other, ok := actual.(bool); return ok && other == boolean }, nil
	}
	number, ok := detectionNumber(value)
	if !ok {
		return nil, errors.New("unsupported field value type; expected string, finite number, boolean, or null")
	}
	return func(actual any) bool { other, ok := detectionNumber(actual); return ok && number.Cmp(other) == 0 }, nil
}

// Event arrays are scalar collections, not arbitrary recursive structures.
// A rule's all modifier may match different members of the same array.
func detectionActual(actual any, match func(any) bool) bool {
	if values, ok := detectionList(actual); ok {
		if len(values) > detectionMaxListItems {
			return false
		}
		for _, value := range values {
			if match(value) {
				return true
			}
		}
		return false
	}
	return match(actual)
}

func detectionList(value any) ([]any, bool) {
	switch values := value.(type) {
	case []any:
		return values, true
	case []string:
		if len(values) > detectionMaxListItems {
			return nil, true
		}
		result := make([]any, len(values))
		for i, value := range values {
			result[i] = value
		}
		return result, true
	case []map[string]any:
		if len(values) > detectionMaxListItems {
			return nil, true
		}
		result := make([]any, len(values))
		for i, value := range values {
			result[i] = value
		}
		return result, true
	default:
		return nil, false
	}
}

func detectionAnd(parts []predicate) predicate {
	return func(fields map[string]any) bool {
		for _, part := range parts {
			if !part(fields) {
				return false
			}
		}
		return true
	}
}

func detectionOr(parts []predicate) predicate {
	return func(fields map[string]any) bool {
		for _, part := range parts {
			if part(fields) {
				return true
			}
		}
		return false
	}
}

func detectionWildcard(value, mode string, cased bool) string {
	var pattern strings.Builder
	if cased {
		pattern.WriteString(`(?s)\A`)
	} else {
		pattern.WriteString(`(?is)\A`)
	}
	if mode == "contains" || mode == "endswith" {
		pattern.WriteString(".*")
	}
	runes := []rune(value)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\\' && i+1 < len(runes) && strings.ContainsRune(`\*?`, runes[i+1]) {
			i++
			pattern.WriteString(regexp.QuoteMeta(string(runes[i])))
		} else if r == '*' {
			pattern.WriteString(".*")
		} else if r == '?' {
			pattern.WriteByte('.')
		} else {
			pattern.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	if mode == "contains" || mode == "startswith" {
		pattern.WriteString(".*")
	}
	pattern.WriteString(`\z`)
	return pattern.String()
}

func detectionRegexSubset(pattern string) error {
	inClass := false
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			if i+1 < len(pattern) {
				i++
				escaped := pattern[i]
				if escaped >= '0' && escaped <= '9' || strings.ContainsRune("kgpPAzZQEC", rune(escaped)) {
					return errors.New("unsupported regular-expression feature: extended escape or backreference")
				}
			}
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '(':
			if !inClass && i+1 < len(pattern) && pattern[i+1] == '?' {
				return errors.New("unsupported regular-expression feature: (? group; use re|i, re|m, re|s for flags")
			}
		}
	}
	return nil
}

var detectionJSONNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func detectionNumber(value any) (*big.Rat, bool) {
	var text string
	switch number := value.(type) {
	case int:
		text = strconv.FormatInt(int64(number), 10)
	case int8:
		text = strconv.FormatInt(int64(number), 10)
	case int16:
		text = strconv.FormatInt(int64(number), 10)
	case int32:
		text = strconv.FormatInt(int64(number), 10)
	case int64:
		text = strconv.FormatInt(number, 10)
	case uint:
		text = strconv.FormatUint(uint64(number), 10)
	case uint8:
		text = strconv.FormatUint(uint64(number), 10)
	case uint16:
		text = strconv.FormatUint(uint64(number), 10)
	case uint32:
		text = strconv.FormatUint(uint64(number), 10)
	case uint64:
		text = strconv.FormatUint(number, 10)
	case float32:
		if math.IsNaN(float64(number)) || math.IsInf(float64(number), 0) {
			return nil, false
		}
		text = strconv.FormatFloat(float64(number), 'g', -1, 32)
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, false
		}
		text = strconv.FormatFloat(number, 'g', -1, 64)
	case json.Number:
		text = string(number)
		if len(text) > 128 || !detectionJSONNumber.MatchString(text) {
			return nil, false
		}
	default:
		return nil, false
	}
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		exponent, err := strconv.Atoi(text[index+1:])
		if err != nil || exponent < -1024 || exponent > 1024 {
			return nil, false
		}
	}
	result, ok := new(big.Rat).SetString(text)
	return result, ok
}

func conditionReserved(name string) bool {
	switch name {
	case "and", "or", "not", "of", "all", "them", "1":
		return true
	default:
		return false
	}
}

func conditionIdentifier(name string, wildcard bool) bool {
	if name == "" || len(name) > 256 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || wildcard && r == '*') {
			return false
		}
	}
	return true
}

func conditionLex(expression string) ([]string, error) {
	if len(expression) == 0 || len(expression) > conditionMaxBytes || !utf8.ValidString(expression) {
		return nil, errors.New("invalid or oversized condition expression")
	}
	var tokens []string
	for i := 0; i < len(expression); {
		if strings.ContainsRune(" \t\r\n", rune(expression[i])) {
			i++
			continue
		}
		if expression[i] == '(' || expression[i] == ')' {
			tokens = append(tokens, expression[i:i+1])
			i++
		} else {
			start := i
			for i < len(expression) && !strings.ContainsRune(" \t\r\n()", rune(expression[i])) {
				i++
			}
			token := expression[start:i]
			if !conditionIdentifier(token, true) {
				return nil, fmt.Errorf("unsupported condition token: %q", token)
			}
			tokens = append(tokens, token)
		}
		if len(tokens) > conditionMaxTokens {
			return nil, errors.New("condition exceeds 1024 tokens")
		}
	}
	if len(tokens) == 0 {
		return nil, errors.New("empty condition expression")
	}
	return tokens, nil
}

type conditionParser struct {
	tokens []string
	position int
	selections map[string]predicate
	names []string
}

func (p *conditionParser) take(token string) bool {
	if p.position < len(p.tokens) && p.tokens[p.position] == token {
		p.position++
		return true
	}
	return false
}

func (p *conditionParser) parseOr(depth int) (predicate, error) {
	first, err := p.parseAnd(depth)
	if err != nil {
		return nil, err
	}
	parts := []predicate{first}
	for p.take("or") {
		next, err := p.parseAnd(depth)
		if err != nil {
			return nil, err
		}
		parts = append(parts, next)
	}
	return detectionOr(parts), nil
}

func (p *conditionParser) parseAnd(depth int) (predicate, error) {
	first, err := p.parseUnary(depth)
	if err != nil {
		return nil, err
	}
	parts := []predicate{first}
	for p.take("and") {
		next, err := p.parseUnary(depth)
		if err != nil {
			return nil, err
		}
		parts = append(parts, next)
	}
	return detectionAnd(parts), nil
}

func (p *conditionParser) parseUnary(depth int) (predicate, error) {
	if depth > conditionMaxDepth {
		return nil, errors.New("condition exceeds nesting depth limit")
	}
	if p.take("not") {
		next, err := p.parseUnary(depth+1)
		if err != nil {
			return nil, err
		}
		return func(fields map[string]any) bool { return !next(fields) }, nil
	}
	if p.take("(") {
		next, err := p.parseOr(depth+1)
		if err != nil {
			return nil, err
		}
		if !p.take(")") {
			return nil, errors.New("condition missing closing parenthesis")
		}
		return next, nil
	}
	if p.position >= len(p.tokens) {
		return nil, errors.New("condition is missing an operand")
	}
	name := p.tokens[p.position]
	p.position++
	if name == "1" || name == "all" {
		if !p.take("of") || p.position >= len(p.tokens) {
			return nil, errors.New("condition selector requires 1/all of a selection pattern")
		}
		pattern := p.tokens[p.position]
		p.position++
		if !conditionIdentifier(pattern, true) || pattern != "them" && conditionReserved(pattern) {
			return nil, errors.New("invalid condition selection pattern")
		}
		matchPattern := pattern
		if pattern == "them" {
			matchPattern = "*"
		}
		compiled, err := regexp.Compile("\\A" + strings.ReplaceAll(regexp.QuoteMeta(matchPattern), `\*`, ".*") + "\\z")
		if err != nil {
			return nil, errors.New("invalid condition selection pattern")
		}
		var parts []predicate
		for _, selection := range p.names {
			if strings.HasPrefix(selection, "_") && !strings.HasPrefix(pattern, "_") {
				continue
			}
			if compiled.MatchString(selection) {
				parts = append(parts, p.selections[selection])
			}
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("condition selector matches no selection: %s", pattern)
		}
		if name == "all" {
			return detectionAnd(parts), nil
		}
		return detectionOr(parts), nil
	}
	if conditionReserved(name) || strings.ContainsRune(name, '*') {
		return nil, fmt.Errorf("unexpected condition operand: %s", name)
	}
	selected, ok := p.selections[name]
	if !ok {
		return nil, fmt.Errorf("unknown condition selection: %s", name)
	}
	return selected, nil
}
