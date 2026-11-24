package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newFlags builds a flag set with the shapes the real one has: a string, a
// bool, a duration and a repeatable value. The tests exercise applyConfig
// against those rather than against main's whole set, which cannot be built
// twice in one process.
func newFlags() (*flag.FlagSet, *string, *bool, *time.Duration, *sectionFlag) {
	set := flag.NewFlagSet("bms", flag.ContinueOnError)
	set.SetOutput(nil)
	db := set.String("db", "bms.db", "")
	web := set.Bool("web", true, "")
	every := set.Duration("backup-every", 24*time.Hour, "")
	var sections sectionFlag
	set.Var(&sections, "section", "")
	set.String("config", "", "")
	return set, db, web, every, &sections
}

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bms.conf")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigSetsEveryKindOfFlag(t *testing.T) {
	set, db, web, every, sections := newFlags()
	path := write(t, `
# A library server.
db = /var/lib/bask-media-server/bms.db
web = false
backup-every = 6h

section = Movies:movies:/mnt/media/Movies
section = TV Shows:shows:/mnt/media/TV Shows
`)
	if err := set.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig(set, path); err != nil {
		t.Fatal(err)
	}
	if *db != "/var/lib/bask-media-server/bms.db" {
		t.Errorf("db = %q", *db)
	}
	if *web {
		t.Error("web should be off")
	}
	if *every != 6*time.Hour {
		t.Errorf("backup-every = %v", *every)
	}
	if len(sections.specs) != 2 {
		t.Fatalf("sections = %v", sections.specs)
	}
	// A title with a space is ordinary, so it has to survive parsing intact.
	if sections.specs[1] != "TV Shows:shows:/mnt/media/TV Shows" {
		t.Errorf("second section = %q", sections.specs[1])
	}
}

func TestCommandLineBeatsTheFile(t *testing.T) {
	set, db, _, _, _ := newFlags()
	if err := set.Parse([]string{"-db", "/tmp/chosen.db"}); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig(set, write(t, "db = /var/lib/ignored.db\n")); err != nil {
		t.Fatal(err)
	}
	if *db != "/tmp/chosen.db" {
		t.Errorf("db = %q; the command line should win", *db)
	}
}

// Sections from both sources would produce a library list that is in neither
// place, so the command line replaces the file's rather than adding to it.
func TestSectionsOnTheCommandLineReplaceTheFile(t *testing.T) {
	set, _, _, _, sections := newFlags()
	if err := set.Parse([]string{"-section", "Only:movies:/mnt/only"}); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig(set, write(t, "section = Films:movies:/mnt/films\n")); err != nil {
		t.Fatal(err)
	}
	if len(sections.specs) != 1 || sections.specs[0] != "Only:movies:/mnt/only" {
		t.Errorf("sections = %v", sections.specs)
	}
}

func TestConfigRefusesWhatItCannotMean(t *testing.T) {
	for _, test := range []struct{ name, body, want string }{
		{"an unknown setting", "colour = blue\n", "no setting called"},
		{"a line that is not a setting", "just some words\n", "want \"name = value\""},
		{"a value a flag rejects", "backup-every = soon\n", "backup-every"},
		{"a section missing its path", "section = Movies:movies\n", "Title:movies"},
		{"another configuration file", "config = /etc/other.conf\n", "cannot name another one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			set, _, _, _, _ := newFlags()
			if err := set.Parse(nil); err != nil {
				t.Fatal(err)
			}
			err := applyConfig(set, write(t, test.body))
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %v; want it to mention %q", err, test.want)
			}
		})
	}
}

// The line number is the whole value of the message: a file with one bad line
// in thirty is the case this has to serve.
func TestConfigNamesTheLine(t *testing.T) {
	set, _, _, _, _ := newFlags()
	if err := set.Parse(nil); err != nil {
		t.Fatal(err)
	}
	err := applyConfig(set, write(t, "# a comment\ndb = /tmp/a.db\n\ncolour = blue\n"))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), ":4:") {
		t.Errorf("error = %v; want it to name line 4", err)
	}
}

func TestQuotesAreOptionalAndLiteralInside(t *testing.T) {
	set, db, _, _, sections := newFlags()
	if err := set.Parse(nil); err != nil {
		t.Fatal(err)
	}
	body := "db = \"/mnt/My Media/bms.db\"\n" +
		"section = 'Films:movies:/mnt/films'\n"
	if err := applyConfig(set, write(t, body)); err != nil {
		t.Fatal(err)
	}
	if *db != "/mnt/My Media/bms.db" {
		t.Errorf("db = %q", *db)
	}
	if sections.specs[0] != "Films:movies:/mnt/films" {
		t.Errorf("section = %q", sections.specs[0])
	}
}

func TestMissingConfigIsAnError(t *testing.T) {
	set, _, _, _, _ := newFlags()
	if err := set.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyConfig(set, filepath.Join(t.TempDir(), "absent.conf")); err == nil {
		t.Fatal("want an error for a file that is not there")
	}
}
