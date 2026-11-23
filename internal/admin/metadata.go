package admin

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/metadata"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// Correcting what a provider got wrong.
//
// Two things, which are the same thing from a household's point of view:
// typing the right title, and picking the right entry out of the provider's
// list. Both were previously impossible — a wrong match was permanent, and
// the only correction available was renaming the file and scanning again.
//
// Anything corrected is locked, so the next metadata pass leaves it alone.
// That is the part that makes a correction worth making: a provider that
// overwrites it tonight is worse than no provider at all.

// itemDetail is one item as the editor shows it.
type itemDetail struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Library string `json:"library,omitempty"`

	Title   string `json:"title"`
	Year    int    `json:"year,omitempty"`
	Summary string `json:"summary,omitempty"`

	// ScannedTitle and ScannedYear are what the filename said. Shown beside
	// the fields so somebody can see what they changed, and what reverting
	// would put back.
	ScannedTitle string `json:"scannedTitle"`
	ScannedYear  int    `json:"scannedYear,omitempty"`

	// Genres are what it is about, so somebody can see what a provider said
	// and correct it. The rest are the words that are a person's: a tag and
	// a label nobody else decides, and the mood and style a music client
	// shows beside an album.
	Genres []string `json:"genres,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	Labels []string `json:"labels,omitempty"`
	Moods  []string `json:"moods,omitempty"`
	Styles []string `json:"styles,omitempty"`

	// Studio and People are what a metadata pass found, shown so somebody
	// can see whether a library that asked for credits actually got them.
	// Read-only here: a cast list is forty names from a catalogue, and
	// correcting one by hand is a different feature from correcting a
	// title.
	Studio string          `json:"studio,omitempty"`
	People []domain.Credit `json:"people,omitempty"`

	// Aired is when an episode first went out, which is what places a
	// special among the seasons when nobody has said where it goes.
	Aired string `json:"aired,omitempty"`

	// Special says this is a season-zero episode, so the editor knows to
	// offer the placement at all; Placement is where it currently sits and
	// who decided that.
	Special   bool           `json:"special,omitempty"`
	Placement *placementView `json:"placement,omitempty"`

	ProviderID string `json:"providerId,omitempty"`
	Locked     bool   `json:"locked"`
	Matched    bool   `json:"matched"`

	// Files are the paths behind it, because the first thing anybody checks
	// about a wrong match is which file it is.
	Files []string `json:"files,omitempty"`

	// CanRematch says whether this server can offer the provider's other
	// entries. False means no key, or a provider that cannot be asked for a
	// list — the editor then offers the fields and says why the rest is
	// missing, rather than a button that fails.
	CanRematch bool `json:"canRematch"`

	// Why is what to tell somebody when CanRematch is false.
	Why string `json:"why,omitempty"`
}

// readItem is the editor's first request.
func (s *Server) readItem(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such item")
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}
	scannedTitle, scannedYear, err := s.DB.ScannedTitle(r.Context(), id)
	if err != nil {
		s.fail(w, "read item", err)
		return
	}
	named, err := s.libraryNames(r)
	if err != nil {
		s.fail(w, "list sections", err)
		return
	}

	files := make([]string, 0, len(item.Parts))
	for _, part := range item.Parts {
		files = append(files, part.Path)
	}

	out := itemDetail{
		ID: item.ID, Kind: string(item.Kind), Library: named[item.SectionID],
		Title: item.Title, Year: item.Year, Summary: item.Summary,
		ScannedTitle: scannedTitle, ScannedYear: scannedYear,
		Genres:     item.Genres,
		Tags:       item.Tags,
		Labels:     item.Labels,
		Moods:      item.Moods,
		Styles:     item.Styles,
		Studio:     item.Studio,
		People:     item.People,
		ProviderID: item.ProviderID, Locked: item.Locked,
		Matched: item.ProviderID != "", Files: files,
	}
	if !item.Aired.IsZero() {
		out.Aired = item.Aired.Format("2006-01-02")
	}
	// Only a special gets the placement controls: a regular episode's place
	// is its season and its number, and offering to move it would be
	// offering to lie about which episode it is.
	if item.Kind == domain.KindEpisode && item.ParentID != nil {
		if season, err := s.DB.Item(r.Context(), *item.ParentID); err == nil && season.Index == 0 {
			out.Special = true
			out.Placement = &placementView{
				AfterSeason:   item.Placement.AfterSeason,
				BeforeSeason:  item.Placement.BeforeSeason,
				BeforeEpisode: item.Placement.BeforeEpisode,
				ByHand:        item.Placement.ByHand,
			}
		}
	}
	if _, err := s.matcher(r.Context(), item.Kind); err != nil {
		out.Why = err.Error()
	} else {
		out.CanRematch = true
	}
	writeJSON(w, http.StatusOK, out)
}

// placementView is where a special sits, as the page reads and writes it.
//
// Minus one means nobody has said, which the page draws as an empty field:
// zero is a real answer — "after season 0" is meaningful — so a sentinel
// that collided with it would make one unsayable.
type placementView struct {
	AfterSeason   int  `json:"afterSeason"`
	BeforeSeason  int  `json:"beforeSeason"`
	BeforeEpisode int  `json:"beforeEpisode"`
	ByHand        bool `json:"byHand"`
}

// placeSpecial records where a special belongs, as somebody's own answer.
//
// Its own route rather than part of the item patch, because it is the one
// correction that must survive a metadata pass without locking everything
// else about the item: a placement set here beats both TheTVDB and the air
// date, and leaves the title and the artwork to be fetched as usual.
func (s *Server) placeSpecial(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	var body placementView
	if !decode(w, r, &body) {
		return
	}

	placement := domain.Placement{
		AfterSeason:   body.AfterSeason,
		BeforeSeason:  body.BeforeSeason,
		BeforeEpisode: body.BeforeEpisode,
	}
	// A placement that names both is contradictory: before season three and
	// after season one describe two different gaps, and guessing which was
	// meant would move somebody's special somewhere they did not ask for.
	if placement.AfterSeason != domain.NoPlacement && placement.BeforeSeason != domain.NoPlacement {
		writeError(w, http.StatusBadRequest,
			"a special goes after one season or before another, not both")
		return
	}
	// An episode without a season to count it in names nothing.
	if placement.BeforeEpisode != domain.NoPlacement && placement.BeforeSeason == domain.NoPlacement {
		writeError(w, http.StatusBadRequest,
			"an episode number needs the season it is in — set the season as well")
		return
	}

	err := s.DB.SetPlacement(r.Context(), id, placement, true)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such item")
		return
	}
	if err != nil {
		s.fail(w, "place the special", err)
		return
	}
	s.logger().Info("placed a special by hand", "item", id,
		"afterSeason", placement.AfterSeason, "beforeSeason", placement.BeforeSeason)
	writeJSON(w, http.StatusOK, map[string]any{
		"note": "Saved. This beats both TheTVDB and the air date, and a metadata pass will not move it.",
	})
}

// editItem applies a correction typed by hand.
func (s *Server) editItem(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		Title   *string   `json:"title"`
		Year    *int      `json:"year"`
		Summary *string   `json:"summary"`
		Genres  *[]string `json:"genres"`
		Tags    *[]string `json:"tags"`
		Labels  *[]string `json:"labels"`
		Moods   *[]string `json:"moods"`
		Styles  *[]string `json:"styles"`
		Locked  *bool     `json:"locked"`
	}
	if !decode(w, r, &body) {
		return
	}
	err := s.DB.EditItem(r.Context(), id, store.Edit{
		Title: body.Title, Year: body.Year, Summary: body.Summary,
		Genres: body.Genres, Tags: body.Tags, Labels: body.Labels,
		Moods: body.Moods, Styles: body.Styles,
		Locked: body.Locked,
	})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such item")
		return
	}
	if err != nil {
		// A title that is only spaces is the one thing this refuses, and the
		// store's message says so plainly.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revertItem puts the filename's own title back and unlocks the item, so the
// next metadata pass treats it as if nobody had ever touched it.
func (s *Server) revertItem(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	title, year, err := s.DB.ScannedTitle(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such item")
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}
	unlocked := false
	if err := s.DB.EditItem(r.Context(), id, store.Edit{
		Title: &title, Year: &year, Locked: &unlocked,
	}); err != nil {
		s.fail(w, "revert item", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// candidateView is one of the provider's entries, as the page lists it.
type candidateView struct {
	ProviderID string `json:"providerId"`
	Title      string `json:"title"`
	Year       int    `json:"year,omitempty"`
	Summary    string `json:"summary,omitempty"`
	PosterURL  string `json:"posterUrl,omitempty"`
	Current    bool   `json:"current,omitempty"`
}

// candidates offers the entries the provider did not pick.
func (s *Server) candidates(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such item")
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}
	matcher, err := s.matcher(r.Context(), item.Kind)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// The search term is whatever is in the box, falling back to the title:
	// a wrong match is often a wrong title, and the fix is searching for
	// something the filename never said.
	term := strings.TrimSpace(r.URL.Query().Get("q"))
	if term == "" {
		term = item.Title
	}
	// A year narrows the search and an empty box does not: somebody
	// correcting a match often does not know the year, and insisting on the
	// scanned one is how the wrong entry stays at the top of the list.
	year := 0
	if value, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("year"))); err == nil && value > 0 {
		year = value
	}

	var found []metadata.Match
	if item.Kind == domain.KindShow {
		found, err = matcher.ShowCandidates(r.Context(), term, year)
	} else {
		found, err = matcher.MovieCandidates(r.Context(), term, year)
	}
	if err != nil {
		s.fail(w, "search the provider", err)
		return
	}

	out := make([]candidateView, 0, len(found))
	for _, match := range found {
		out = append(out, candidateView{
			ProviderID: match.ProviderID, Title: match.Title, Year: match.Year,
			Summary: match.Summary, PosterURL: match.PosterURL,
			Current: match.ProviderID != "" && match.ProviderID == item.ProviderID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": out, "searched": term})
}

// rematchItem points an item at the entry somebody chose.
//
// The old summary and artwork go first and the new ones are fetched here, so
// the item is never half of one match and half of another. Everything under a
// show is made unmatched as well: an episode is addressed through its show's
// provider id, so a show matched to the wrong series had every episode wrong
// too, and the next metadata pass is what fixes them.
func (s *Server) rematchItem(w http.ResponseWriter, r *http.Request) {
	id, ok := idFrom(w, r)
	if !ok {
		return
	}
	var body struct {
		ProviderID string `json:"providerId"`
	}
	if !decode(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.ProviderID) == "" {
		writeError(w, http.StatusBadRequest, "which entry")
		return
	}

	item, err := s.DB.Item(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such item")
		return
	}
	if err != nil {
		s.fail(w, "read item", err)
		return
	}
	matcher, err := s.matcher(r.Context(), item.Kind)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	var match metadata.Match
	if item.Kind == domain.KindShow {
		match, err = matcher.ShowByID(r.Context(), body.ProviderID)
	} else {
		match, err = matcher.MovieByID(r.Context(), body.ProviderID)
	}
	if errors.Is(err, metadata.ErrNoMatch) {
		writeError(w, http.StatusNotFound, "the provider has nothing under that id")
		return
	}
	if err != nil {
		s.fail(w, "fetch the match", err)
		return
	}

	if err := s.DB.Rematch(r.Context(), id, match.ProviderID); err != nil {
		s.fail(w, "rematch", err)
		return
	}

	posterKey, artKey := "", ""
	if s.Artwork != nil {
		if match.PosterURL != "" {
			if key, err := s.Artwork.Fetch(r.Context(), match.PosterURL); err == nil {
				posterKey = key
			} else {
				s.logger().Warn("the new poster did not fetch", "item", id, "error", err)
			}
		}
		if match.BackdropURL != "" {
			if key, err := s.Artwork.Fetch(r.Context(), match.BackdropURL); err == nil {
				artKey = key
			} else {
				s.logger().Warn("the new backdrop did not fetch", "item", id, "error", err)
			}
		}
	}
	if err := s.DB.SaveMetadata(r.Context(), id, match.ProviderID, match.Summary, posterKey, artKey); err != nil {
		s.fail(w, "save the match", err)
		return
	}
	// The rating comes with the match, and a correction that left the old
	// one in place would be a film re-matched to a children's title that is
	// still rated 18 — or the other way round, which matters more.
	if match.Rating != "" {
		if err := s.DB.SetRating(r.Context(), id, match.Rating); err != nil {
			s.fail(w, "save the rating", err)
			return
		}
	}

	note := ""
	if item.Kind == domain.KindShow {
		if err := s.DB.ForgetDescendants(r.Context(), id); err != nil {
			s.fail(w, "forget the episodes", err)
			return
		}
		// The seasons and episodes take the series' rating now rather than
		// at the next pass, because the restriction that reads it is
		// enforced on the next request.
		if err := s.DB.InheritRatings(r.Context()); err != nil {
			s.fail(w, "hand the rating down", err)
			return
		}
		note = "The episodes were matched through the old series, so they are unmatched again: run a metadata pass to fetch them."
	}
	s.logger().Info("rematched an item",
		"item", id, "kind", item.Kind, "provider", match.ProviderID, "title", match.Title)

	writeJSON(w, http.StatusOK, map[string]any{
		"providerId": match.ProviderID,
		"title":      match.Title,
		"year":       match.Year,
		"note":       note,
	})
}

// matcher is the provider, if it can be asked for a list of entries.
//
// Resolved per request rather than held, like everywhere else here: the key
// can be saved from this very page a moment earlier. The error is written for
// somebody reading it on the page, because that is where it goes.
func (s *Server) matcher(ctx context.Context, kind domain.Kind) (metadata.Matcher, error) {
	switch kind {
	case domain.KindMovie, domain.KindShow:
	default:
		return nil, errors.New("only a film or a series can be matched to a provider")
	}
	if s.Jobs == nil || s.Jobs.Provider == nil {
		return nil, errors.New("this server has no metadata provider")
	}
	provider, err := s.Jobs.Provider(ctx)
	if err != nil {
		return nil, err
	}
	matcher, ok := provider.(metadata.Matcher)
	if !ok {
		return nil, errors.New("this provider cannot be asked for its other entries")
	}
	return matcher, nil
}
