package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type gqlVariable string
type gqlField struct {
	name, alias, condition string
	args                   record
	fields                 []gqlField
	directives             []gqlDirective
}
type gqlDirective struct {
	name  string
	value any
}
type gqlOperation struct {
	name, kind string
	defaults   record
	fields     []gqlField
}
type gqlParser struct {
	tokens    []string
	index     int
	err       error
	fragments map[string]gqlField
}

func gqlTokens(query string) ([]string, error) {
	var tokens []string
	name := func(ch byte) bool {
		return ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9'
	}
	for pos := 0; pos < len(query); {
		start := pos
		ch := query[pos]
		pos++
		switch {
		case ch == ' ' || ch == '\n' || ch == '\r' || ch == '\t' || ch == ',':
			continue
		case ch == '#':
			for pos < len(query) && query[pos] != '\n' {
				pos++
			}
			continue
		case ch == '.' && strings.HasPrefix(query[start:], "..."):
			pos = start + 3
		case ch == '"':
			if strings.HasPrefix(query[start:], `"""`) {
				end := strings.Index(query[start+3:], `"""`)
				if end < 0 {
					return nil, fmt.Errorf("unterminated block string")
				}
				pos = start + 6 + end
				tokens = append(tokens, strconv.Quote(query[start+3:pos-3]))
				continue
			}
			for pos < len(query) && query[pos] != '"' {
				if query[pos] == '\\' {
					pos++
				}
				pos++
			}
			if pos >= len(query) {
				return nil, fmt.Errorf("unterminated string")
			}
			pos++
		case name(ch) || ch == '-':
			for pos < len(query) && (name(query[pos]) || (ch == '-' || ch >= '0' && ch <= '9') && strings.ContainsRune(".+-", rune(query[pos]))) {
				pos++
			}
		case strings.ContainsRune("!$():=@[]{|}&", rune(ch)):
		default:
			return nil, fmt.Errorf("unexpected character %q", ch)
		}
		tokens = append(tokens, query[start:pos])
		if len(tokens) > 50000 {
			return nil, fmt.Errorf("document too large")
		}
	}
	return tokens, nil
}
func (parser *gqlParser) peek() string {
	if parser.index >= len(parser.tokens) {
		return ""
	}
	return parser.tokens[parser.index]
}
func (parser *gqlParser) pop() string {
	token := parser.peek()
	if token != "" {
		parser.index++
	}
	return token
}
func (parser *gqlParser) match(token string) bool {
	if parser.peek() != token {
		return false
	}
	parser.pop()
	return true
}
func (parser *gqlParser) expect(token string) {
	if !parser.match(token) && parser.err == nil {
		parser.err = fmt.Errorf("expected %q, got %q", token, parser.peek())
	}
}
func (parser *gqlParser) name() string {
	value := parser.pop()
	if value == "" || !(value[0] == '_' || value[0] >= 'A' && value[0] <= 'Z' || value[0] >= 'a' && value[0] <= 'z') {
		parser.err = fmt.Errorf("expected name, got %q", value)
	}
	return value
}
func (parser *gqlParser) args(end string, depth int) record {
	args := record{}
	for parser.err == nil && !parser.match(end) {
		key := parser.name()
		parser.expect(":")
		args[key] = parser.value(depth + 1)
	}
	return args
}
func (parser *gqlParser) value(depth int) any {
	if depth > 80 {
		parser.err = fmt.Errorf("input nesting too deep")
		return nil
	}
	token := parser.pop()
	switch token {
	case "$":
		return gqlVariable(parser.name())
	case "{":
		return parser.args("}", depth)
	case "[":
		values := []any{}
		for parser.err == nil && !parser.match("]") {
			values = append(values, parser.value(depth+1))
		}
		return values
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	case "":
		parser.err = fmt.Errorf("expected value")
		return nil
	}
	if strings.HasPrefix(token, `"`) {
		var value string
		if err := json.Unmarshal([]byte(token), &value); err != nil {
			parser.err = err
		}
		return value
	}
	if value, err := strconv.Atoi(token); err == nil {
		return value
	}
	if value, err := strconv.ParseFloat(token, 64); err == nil {
		return value
	}
	return token
}
func (parser *gqlParser) directives() []gqlDirective {
	var directives []gqlDirective
	for parser.err == nil && parser.match("@") {
		name := parser.name()
		var value any
		if parser.match("(") {
			value = parser.args(")", 0)["if"]
		}
		directives = append(directives, gqlDirective{name, value})
	}
	return directives
}
func (parser *gqlParser) selection(depth int) []gqlField {
	if depth > 80 {
		parser.err = fmt.Errorf("selection nesting too deep")
		return nil
	}
	parser.expect("{")
	fields := []gqlField{}
	for parser.err == nil && !parser.match("}") {
		var field gqlField
		if parser.match("...") {
			if parser.match("on") {
				field.condition = parser.name()
			} else if parser.peek() != "@" && parser.peek() != "{" {
				field.alias = parser.name()
			}
		} else {
			field.name = parser.name()
			field.alias = field.name
			if parser.match(":") {
				field.name = parser.name()
			}
			if parser.match("(") {
				field.args = parser.args(")", 0)
			}
		}
		field.directives = parser.directives()
		if parser.peek() == "{" {
			field.fields = parser.selection(depth + 1)
		}
		fields = append(fields, field)
	}
	return fields
}
func (parser *gqlParser) typeRef(depth int) {
	if depth > 30 {
		parser.err = fmt.Errorf("type nesting too deep")
		return
	}
	if parser.match("[") {
		parser.typeRef(depth + 1)
		parser.expect("]")
	} else {
		parser.name()
	}
	parser.match("!")
}
func gqlParse(query string) ([]gqlOperation, map[string]gqlField, error) {
	tokens, err := gqlTokens(query)
	if err != nil {
		return nil, nil, err
	}
	parser := &gqlParser{tokens: tokens, fragments: map[string]gqlField{}}
	var operations []gqlOperation
	for parser.peek() != "" && parser.err == nil {
		if parser.match("fragment") {
			name := parser.name()
			parser.expect("on")
			condition := parser.name()
			directives := parser.directives()
			parser.fragments[name] = gqlField{condition: condition, fields: parser.selection(0), directives: directives}
			continue
		}
		operation := gqlOperation{kind: "query", defaults: record{}}
		if parser.peek() != "{" {
			operation.kind = parser.name()
			if operation.kind != "query" && operation.kind != "mutation" && operation.kind != "subscription" {
				return nil, nil, fmt.Errorf("unknown operation %q", operation.kind)
			}
			if name := parser.peek(); name != "{" && name != "(" && name != "@" {
				operation.name = parser.name()
			}
			if parser.match("(") {
				for parser.err == nil && !parser.match(")") {
					parser.expect("$")
					name := parser.name()
					parser.expect(":")
					parser.typeRef(0)
					if parser.match("=") {
						operation.defaults[name] = parser.value(0)
					}
					parser.directives()
				}
			}
			parser.directives()
		}
		operation.fields = parser.selection(0)
		operations = append(operations, operation)
	}
	if parser.err == nil && len(operations) == 0 {
		parser.err = fmt.Errorf("no GraphQL operation")
	}
	return operations, parser.fragments, parser.err
}
func (exec *gqlExecution) value(value any) any {
	switch typed := value.(type) {
	case gqlVariable:
		return exec.variables[string(typed)]
	case record:
		result := record{}
		for key, entry := range typed {
			result[key] = exec.value(entry)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, entry := range typed {
			result = append(result, exec.value(entry))
		}
		return result
	}
	return value
}
func objectRecord(value any) record {
	switch typed := value.(type) {
	case record:
		return typed
	case map[string]any:
		return record(typed)
	}
	return nil
}
