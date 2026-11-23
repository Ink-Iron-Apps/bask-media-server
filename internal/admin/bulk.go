package admin

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/inknironapps/bask-media-server/internal/store"
)

// Changing the same thing about many items at once.
//
// The one-at-a-time editor is right for a wrong title. This is for the other
// job: a folder of forty films that all want the same label, a library whose
// certificates all came out blank. Doing that forty times through a page is
// the work people write scripts to avoid, and then the script is the thing
// with no validation in it.

// bulkRequest is what the page sends.
//
// Words are added and removed rather than replaced, which is the whole point
// of a bulk edit over a search result: "also label these Christmas". A
// replace would mean every other label on every one of them was lost to a
// field somebody left empty, and replacing is available one item at a time
// where the page can show what is there now.
type bulkRequest struct {
	Items []int64 `json:"items"`

	AddGenres    []string `json:"addGenres"`
	RemoveGenres []string `json:"removeGenres"`
	AddTags      []string `json:"addTags"`
	RemoveTags   []string `json:"removeTags"`
	AddLabels    []string `json:"addLabels"`
	RemoveLabels []string `json:"removeLabels"`
	AddMoods     []string `json:"addMoods"`
	RemoveMoods  []string `json:"removeMoods"`
	AddStyles    []string `json:"addStyles"`
	RemoveStyles []string `json:"removeStyles"`

	// Absent means leave it alone; present means set it to this, including
	// to empty. A title, a year and a summary are deliberately absent from
	// this request: forty items with one title is forty items nobody can
	// tell apart.
	Studio *string `json:"studio"`
	Rating *string `json:"rating"`
	Locked *bool   `json:"locked"`
}

// bulkEdit applies one change to many items.
func (s *Server) bulkEdit(w http.ResponseWriter, r *http.Request) {
	var body bulkRequest
	if !decode(w, r, &body) {
		return
	}

	edit := store.BulkEdit{
		AddWords: map[store.TagKind][]string{
			store.TagGenre: body.AddGenres,
			store.TagTag:   body.AddTags,
			store.TagLabel: body.AddLabels,
			store.TagMood:  body.AddMoods,
			store.TagStyle: body.AddStyles,
		},
		RemoveWords: map[store.TagKind][]string{
			store.TagGenre: body.RemoveGenres,
			store.TagTag:   body.RemoveTags,
			store.TagLabel: body.RemoveLabels,
			store.TagMood:  body.RemoveMoods,
			store.TagStyle: body.RemoveStyles,
		},
		Studio: body.Studio,
		Rating: body.Rating,
		Locked: body.Locked,
	}

	touched, err := s.DB.BulkEditItems(r.Context(), body.Items, edit)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// One id that is not there fails the whole batch, because half an
		// applied bulk edit is worse than none: nobody could tell where it
		// stopped, and running it again would double the additions on the
		// items it already reached.
		writeError(w, http.StatusNotFound, "one of those items is no longer there — nothing was changed")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.logger().Info("edited items in bulk", "items", touched)
	writeJSON(w, http.StatusOK, map[string]any{
		"touched": touched,
		"note":    note(touched, body),
	})
}

// note is what the page says afterwards, in the words of what was actually
// asked for.
func note(touched int, body bulkRequest) string {
	what := "Changed"
	if body.Locked != nil && *body.Locked {
		// The one change worth naming, because it is the one that stops
		// something happening later rather than altering what is there.
		what = "Locked"
	}
	if touched == 1 {
		return what + " 1 item."
	}
	return what + " " + strconv.Itoa(touched) + " items."
}
