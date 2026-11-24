package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
)

// applyConfig reads a configuration file of "name = value" lines and applies
// each one to set, using the same names as the command-line flags so there is
// one vocabulary to learn rather than two.
//
// A flag given on the command line wins. That ordering is what makes a service
// installation workable: the unit file names a configuration file, and an
// operator can still override one setting for a single run without editing it.
//
// Repeatable flags accumulate, so the file's sections are added in the order
// they appear — but only when the command line named none, because mixing the
// two sources would quietly produce a library list that is in neither.
func applyConfig(set *flag.FlagSet, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open the configuration: %w", err)
	}
	defer file.Close()

	fromCommandLine := map[string]bool{}
	set.Visit(func(f *flag.Flag) { fromCommandLine[f.Name] = true })

	lines := bufio.NewScanner(file)
	for number := 1; lines.Scan(); number++ {
		name, value, err := parseSetting(lines.Text())
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, number, err)
		}
		if name == "" {
			continue
		}
		if name == "config" {
			return fmt.Errorf("%s:%d: a configuration file cannot name another one", path, number)
		}
		if set.Lookup(name) == nil {
			return fmt.Errorf("%s:%d: there is no setting called %q", path, number, name)
		}
		if fromCommandLine[name] {
			continue
		}
		if err := set.Set(name, value); err != nil {
			return fmt.Errorf("%s:%d: %s: %w", path, number, name, err)
		}
	}
	if err := lines.Err(); err != nil {
		return fmt.Errorf("read the configuration: %w", err)
	}
	return nil
}

// parseSetting reads one line. It returns an empty name for a blank line or a
// comment, so the caller can skip it without a second check.
//
// A value keeps its inner spaces — "TV Shows:shows:/mnt/media/TV Shows" is an
// ordinary library title — and loses only surrounding whitespace and one pair
// of quotes, which is what somebody who has quoted a path on the command line
// will reach for here too.
func parseSetting(line string) (name, value string, err error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
		return "", "", nil
	}
	key, rest, found := strings.Cut(trimmed, "=")
	if !found {
		return "", "", fmt.Errorf("want \"name = value\", got %q", trimmed)
	}
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "-"))
	if name == "" {
		return "", "", fmt.Errorf("a setting needs a name, got %q", trimmed)
	}
	return name, unquote(strings.TrimSpace(rest)), nil
}

// unquote drops one matched pair of surrounding quotes. It is deliberately not
// a shell parser: a value is taken literally, so a quote inside the value or a
// backslash stays exactly as written.
func unquote(value string) string {
	if len(value) < 2 {
		return value
	}
	first, last := value[0], value[len(value)-1]
	if first == last && (first == '"' || first == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}
