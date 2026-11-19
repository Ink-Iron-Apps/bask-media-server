package admin_test

import (
	"os"
	"strings"
	"testing"
)

// The page is hand-written and compiled into the binary, so nothing between
// writing it and a browser loading it would notice a missing bracket: there is
// no bundler and no type checker in the way, by design. A browser is the only
// real parser for it, but the mistake a hand-edited page actually makes is an
// unbalanced delimiter — one too few closing parentheses at the end of a
// nested call — and that is cheap to catch here.
//
// This caught exactly that, once: the page had shipped unparseable and every
// test still passed, because serving a broken asset is still serving it.
func TestTheEmbeddedPageHasBalancedDelimiters(t *testing.T) {
	for _, file := range []string{"web/app.js", "web/app.css"} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if depth, line := imbalance(string(source)); depth != 0 {
			t.Errorf("%s: %+d unclosed delimiters, last one opened at line %d", file, depth, line)
		}
	}
}

// imbalance counts delimiters outside strings and comments, returning the
// depth left over and the line the oldest unclosed one is on.
//
// Not a JavaScript parser: it knows strings, template literals and comments,
// which is enough for this file because it contains no regular expression
// literals — and if one is ever added, this test says so by failing.
func imbalance(source string) (depth, line int) {
	var (
		stack []int // the line each open delimiter is on
		row   = 1
	)
	for i := 0; i < len(source); i++ {
		c := source[i]
		switch {
		case c == '\n':
			row++
		case c == '/' && i+1 < len(source) && source[i+1] == '/':
			for i < len(source) && source[i] != '\n' {
				i++
			}
			row++
		case c == '/' && i+1 < len(source) && source[i+1] == '*':
			i += 2
			for i+1 < len(source) && !(source[i] == '*' && source[i+1] == '/') {
				if source[i] == '\n' {
					row++
				}
				i++
			}
			i++
		case c == '\'' || c == '"' || c == '`':
			quote := c
			i++
			for i < len(source) && source[i] != quote {
				if source[i] == '\\' {
					i++
				} else if source[i] == '\n' {
					row++
				}
				i++
			}
		case strings.ContainsRune("([{", rune(c)):
			stack = append(stack, row)
		case strings.ContainsRune(")]}", rune(c)):
			if len(stack) == 0 {
				return -1, row
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) == 0 {
		return 0, 0
	}
	return len(stack), stack[0]
}

// The page declares a content security policy that forbids inline styles, so
// the script must not set one: a width assigned as element.style is silently
// blocked by the browser, which is a progress bar that never moves and no
// error anywhere but the developer console. Anything that varies at runtime
// is an attribute an element understands — a <progress> value — or a class.
func TestTheScriptSetsNoInlineStyles(t *testing.T) {
	source, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{".style.", "style:", "setAttribute('style'", "cssText"} {
		if strings.Contains(string(source), forbidden) {
			t.Errorf("the script uses %q, which the content security policy blocks", forbidden)
		}
	}
}
