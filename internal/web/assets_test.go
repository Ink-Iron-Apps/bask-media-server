package web_test

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// The same guard the admin interface has, for the same reason: this page is
// hand-written and compiled in, so nothing between writing it and a browser
// loading it would notice a missing bracket. A browser is the only real
// parser; an unbalanced delimiter is the mistake it actually makes.
func TestTheEmbeddedPageHasBalancedDelimiters(t *testing.T) {
	for _, file := range []string{"web/app.js", "web/app.css"} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if depth, line := imbalance(string(source)); depth != 0 {
			t.Errorf("%s: %+d unclosed delimiters, the oldest opened at line %d", file, depth, line)
		}
	}
}

func imbalance(source string) (depth, line int) {
	var (
		stack []int
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

func TestThePageAndItsAssetsAreEmbedded(t *testing.T) {
	h := newHarness(t)

	for path, contentType := range map[string]string{
		"/web/app.js":  "text/javascript",
		"/web/app.css": "text/css",
	} {
		res, _ := h.call(http.MethodGet, path, nil)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, res.StatusCode)
		}
		if !strings.Contains(res.Header.Get("Content-Type"), contentType) {
			t.Errorf("%s served as %q", path, res.Header.Get("Content-Type"))
		}
	}
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
