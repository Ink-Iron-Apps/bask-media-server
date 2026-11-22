package admin

import (
	"net/http"
	"strconv"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// The activity log, in the interface.
//
// Administrator only, like everything else here, and worth saying why: this
// is the most personal thing the server holds. Now Playing shows what is
// happening while it happens, which somebody in the house can see by walking
// into the room; a log answers what somebody watched three Tuesdays ago,
// which they cannot. So it is one page, behind the same sign-in as the
// accounts, with a button that forgets it.

// historyPage caps one page of the log. Enough to read an evening in one
// request, bounded because this table is the one that grows for ever.
const historyPage = 50

// watchView is one watch as the page draws it.
type watchView struct {
	ID       int64  `json:"id"`
	User     string `json:"user"`
	UserID   int64  `json:"userId,omitempty"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	ItemID   int64  `json:"itemId,omitempty"`
	Kind     string `json:"kind,omitempty"`

	Started string `json:"started"`
	Ended   string `json:"ended"`

	// Watched, Paused, Position and Length are seconds, because the page
	// formats them and a duration in nanoseconds is a number nobody reads.
	Watched  int `json:"watched"`
	Paused   int `json:"paused,omitempty"`
	Position int `json:"position,omitempty"`
	Length   int `json:"length,omitempty"`

	Device   string `json:"device,omitempty"`
	Client   string `json:"client,omitempty"`
	Surface  string `json:"surface,omitempty"`
	Address  string `json:"address,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Complete bool   `json:"complete"`
}

func viewWatch(watch domain.Watch) watchView {
	view := watchView{
		ID: watch.ID, User: watch.Name, UserID: watch.UserID,
		Title: watch.Title, Subtitle: watch.Subtitle, ItemID: watch.ItemID,
		Kind:    string(watch.Kind),
		Started: watch.Started.Format(time.RFC3339), Ended: watch.Ended.Format(time.RFC3339),
		Watched: int(watch.Watched().Seconds()), Paused: int(watch.Paused.Seconds()),
		Position: int(watch.Position.Seconds()), Length: int(watch.Length.Seconds()),
		Device: watch.Device, Client: watch.Client, Surface: watch.Surface,
		Address: watch.Address, Mode: watch.Mode, Reason: watch.Reason,
		Complete: watch.Complete,
	}
	if view.User == "" {
		// An account that was deleted, on a server where the name was never
		// recorded. Better than an empty column.
		view.User = "somebody"
	}
	return view
}

// totalView is one line of the summary.
type totalView struct {
	Name    string `json:"name"`
	UserID  int64  `json:"userId,omitempty"`
	ItemID  int64  `json:"itemId,omitempty"`
	Plays   int    `json:"plays"`
	Watched int    `json:"watched"`
}

// listHistory is what was watched, newest first, with the totals for the
// window the page is showing.
func (s *Server) listHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.HistoryFilter{
		UserID: number(q.Get("user")),
		ItemID: number(q.Get("item")),
	}
	days := int(number(q.Get("days")))
	if days < 0 {
		days = 0
	}
	if days > 0 {
		filter.Since = time.Now().UTC().AddDate(0, 0, -days)
	}

	page := store.Page{Start: int(number(q.Get("start"))), Size: historyPage}
	watches, total, err := s.DB.History(r.Context(), filter, page)
	if err != nil {
		s.fail(w, "read history", err)
		return
	}
	out := make([]watchView, 0, len(watches))
	for _, watch := range watches {
		out = append(out, viewWatch(watch))
	}

	// The summary is over the window being read, not over the page: "this
	// month" is the question, and page two of it is not a different answer.
	since := filter.Since
	if since.IsZero() {
		since = time.Unix(0, 0).UTC()
	}
	viewers, err := s.DB.WatchersSince(r.Context(), since)
	if err != nil {
		s.fail(w, "total history", err)
		return
	}
	titles, err := s.DB.TitlesSince(r.Context(), since, 10)
	if err != nil {
		s.fail(w, "total history", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"watches": out,
		"total":   total,
		"start":   page.Start,
		"size":    historyPage,
		"days":    days,
		"viewers": totalViews(viewers),
		"titles":  totalViews(titles),
	})
}

// forgetHistory removes the log, or the part of it older than a number of
// days.
//
// A delete rather than a retention setting, because the question somebody
// actually has is "get rid of it", and a server that quietly forgot things on
// a schedule nobody remembered setting would be worse than one that keeps
// everything until asked.
func (s *Server) forgetHistory(w http.ResponseWriter, r *http.Request) {
	var before time.Time
	if days := int(number(r.URL.Query().Get("olderThan"))); days > 0 {
		before = time.Now().UTC().AddDate(0, 0, -days)
	}
	removed, err := s.DB.ForgetHistory(r.Context(), before)
	if err != nil {
		s.fail(w, "forget history", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

func totalViews(totals []domain.WatchTotal) []totalView {
	out := make([]totalView, 0, len(totals))
	for _, total := range totals {
		name := total.Name
		if name == "" {
			name = total.Title
		}
		out = append(out, totalView{
			Name: name, UserID: total.UserID, ItemID: total.ItemID,
			Plays: total.Plays, Watched: int(total.Watched.Seconds()),
		})
	}
	return out
}

// number is a query parameter as an integer, or zero. A bad number is zero
// rather than a refusal: these are all narrowings of a list, and the useful
// answer to "user=banana" is the unfiltered list rather than an error page.
func number(raw string) int64 {
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}
