package telemetry

import (
	"regexp"
	"strings"
	"testing"
)

// matches evaluates a Cloud Logging filter against one entry, so the tests judge
// the very string the sink is given. It knows only the subset the filter uses:
// field="x", field=("x" OR "y"), log_id("x"), NOT, AND, OR and parentheses.
// Anything else fails the test, and so does AND next to OR without parentheses:
// in this language OR binds tighter, the opposite of what a reader assumes.
//
// cicd/pkg/telemetry has the same file.
func matches(t *testing.T, filter string, e map[string]string) bool {
	t.Helper()
	p := &lqlParser{t: t, toks: lqlTokens(t, filter), entry: e}
	got := p.expr()
	if p.pos != len(p.toks) {
		t.Fatalf("filter: stray %q after a complete expression", p.toks[p.pos])
	}
	return got
}

var lqlToken = regexp.MustCompile(`\(|\)|=|"[^"]*"|[A-Za-z_][A-Za-z_.]*`)

// lqlTokens fails on anything the pattern skips, such as a `:` operator.
func lqlTokens(t *testing.T, filter string) []string {
	t.Helper()
	toks := lqlToken.FindAllString(filter, -1)
	if rest := lqlToken.ReplaceAllString(filter, ""); strings.TrimSpace(rest) != "" {
		t.Fatalf("filter: syntax the test does not know: %q", strings.TrimSpace(rest))
	}
	return toks
}

type lqlParser struct {
	t     *testing.T
	toks  []string
	pos   int
	entry map[string]string
}

func (p *lqlParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *lqlParser) next() string {
	tok := p.peek()
	p.pos++
	return tok
}

func (p *lqlParser) expect(want string) {
	p.t.Helper()
	if got := p.next(); got != want {
		p.t.Fatalf("filter: got %q, want %q", got, want)
	}
}

// str reads a quoted string and returns what is inside the quotes.
func (p *lqlParser) str() string {
	p.t.Helper()
	tok := p.next()
	if len(tok) < 2 || tok[0] != '"' {
		p.t.Fatalf("filter: got %q, want a quoted string", tok)
	}
	return tok[1 : len(tok)-1]
}

// expr is one or more units joined by a single kind of operator.
func (p *lqlParser) expr() bool {
	p.t.Helper()
	got := p.unit()
	op := ""
	for p.peek() == "AND" || p.peek() == "OR" {
		this := p.next()
		if op != "" && this != op {
			p.t.Fatalf("filter: AND and OR mixed without parentheses")
		}
		op = this
		// unit() runs first, so the whole filter parses whatever got is.
		right := p.unit()
		if op == "AND" {
			got = got && right
		} else {
			got = got || right
		}
	}
	return got
}

func (p *lqlParser) unit() bool {
	p.t.Helper()
	switch tok := p.next(); tok {
	case "NOT":
		return !p.unit()
	case "(":
		got := p.expr()
		p.expect(")")
		return got
	case "log_id":
		p.expect("(")
		id := p.str()
		p.expect(")")
		return p.entry["log_id"] == id
	default:
		p.expect("=")
		return p.anyOf(p.entry[tok])
	}
}

// anyOf reads "x" or ("x" OR "y") and says whether value is among them.
func (p *lqlParser) anyOf(value string) bool {
	p.t.Helper()
	if p.peek() != "(" {
		return p.str() == value
	}
	p.next()
	found := p.str() == value
	for p.peek() == "OR" {
		p.next()
		found = (p.str() == value) || found
	}
	p.expect(")")
	return found
}

// The evaluator is what the filter tests stand on, so it gets tests of its own.
func TestEvaluator(t *testing.T) {
	e := map[string]string{"a": "1", "b": "2", "log_id": "x/y"}
	for _, tc := range []struct {
		filter string
		want   bool
	}{
		{`a="1"`, true},
		{`a="2"`, false},
		{`missing="1"`, false},
		{`a=("0" OR "1")`, true},
		{`a=("0" OR "2")`, false},
		{`a="1" AND b="2"`, true},
		{`a="1" AND b="9"`, false},
		{`a="9" OR b="2"`, true},
		{`NOT a="9"`, true},
		{`(a="9" OR b="2") AND log_id("x/y")`, true},
		{`(a="9" OR b="2") AND log_id("x/z")`, false},
		{`(a="9" AND b="2") OR (a="1" AND b="2")`, true},
	} {
		if got := matches(t, tc.filter, e); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.filter, got, tc.want)
		}
	}
}
