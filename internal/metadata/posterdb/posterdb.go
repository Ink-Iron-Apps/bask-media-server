// Package posterdb turns a ThePosterDB link into the image behind it.
//
// Not an artwork source, and that is the whole point of this package
// existing separately: ThePosterDB has no public search API, and its terms do
// not permit crawling its pages. So there is nothing here that browses, and
// building a chooser over it would mean scraping a site that has asked not to
// be scraped.
//
// What it does have is an asset route that serves one picture by its id —
// theposterdb.com/api/assets/{id} — which is what every tool in this
// ecosystem uses and what a person pasting a link already has in front of
// them. So the interaction is: somebody finds a poster on the site, copies
// the address, and pastes it in. This package's only job is to accept the
// several shapes that address comes in and hand back the one that is an
// image.
//
// It is also why the chooser labels this source "from a link" rather than
// offering to search it. A menu that cannot be filled is worse than a box
// that can.
package posterdb

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// Name is what the editor calls this source.
const Name = "theposterdb"

// assetURL is the route that serves a picture by id.
const assetURL = "https://theposterdb.com/api/assets/"

// host is the only site this package claims to understand.
const host = "theposterdb.com"

// idPattern is the number in a poster or asset address. Anchored at the end
// because both shapes put the id last, and bounded because an id is a number
// and nothing else — a path segment that is not one is not a poster.
var idPattern = regexp.MustCompile(`^(\d{1,12})$`)

// ErrNotAPoster means the address is not one this package can turn into an
// image.
//
// Returned rather than guessed at. A link to a set, a user's page or the site
// itself names many pictures or none, and picking one of them would be this
// program deciding which poster somebody wanted.
var ErrNotAPoster = errors.New("that is not a link to one poster")

// Mine reports whether an address is a ThePosterDB one at all, so a caller
// can tell "a link to another site" from "a link to this site that I cannot
// read".
func Mine(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	name := strings.ToLower(parsed.Hostname())
	return name == host || strings.HasSuffix(name, "."+host)
}

// Image is the address of the picture a link names.
//
// Accepts the three shapes somebody is likely to have in the clipboard: a
// poster's own page, the asset route itself, and a bare id typed by hand. A
// set's address is refused, with a message that says why, because a set is
// many posters and this is a box for one.
func Image(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrNotAPoster
	}

	// A bare number, which is what somebody who has done this before types.
	if idPattern.MatchString(raw) {
		return assetURL + raw, nil
	}
	if !Mine(raw) {
		return "", ErrNotAPoster
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", ErrNotAPoster
	}
	parts := make([]string, 0, 4)
	for _, part := range strings.Split(parsed.Path, "/") {
		if part != "" {
			parts = append(parts, strings.ToLower(part))
		}
	}
	if len(parts) == 0 {
		return "", ErrNotAPoster
	}

	last := parts[len(parts)-1]
	if !idPattern.MatchString(last) {
		return "", ErrNotAPoster
	}
	switch {
	case len(parts) >= 3 && parts[0] == "api" && parts[1] == "assets":
		// Already the image.
		return assetURL + last, nil
	case parts[0] == "poster":
		return assetURL + last, nil
	case parts[0] == "set":
		return "", errors.New("that is a set of posters — open the one you want and copy its address")
	}
	return "", ErrNotAPoster
}
