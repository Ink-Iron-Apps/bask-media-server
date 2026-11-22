package jellyfin_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/inknironapps/bask-media-server/internal/history"
	"github.com/inknironapps/bask-media-server/internal/nowplaying"
	"github.com/inknironapps/bask-media-server/internal/store"
)

// The whole chain, from the requests a real client makes to the row in the
// database: a client reports where it is, the register notices a stream, the
// recorder opens a row, the client says it has stopped, the row is closed.
//
// Here rather than in the history package because this is the only place the
// first link exists. Everything after the register is tested by that
// package's own tests; this is the test that they are joined up.
func TestAClientPressingPlayReachesTheLog(t *testing.T) {
	server, token, db := library(t)

	recorder := history.New(history.Options{DB: db})
	server.Playing = nowplaying.New(nowplaying.Options{Changes: recorder.Record})

	c := call{t: t, server: server, token: token}
	films := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Films"]["Id"].(string))
	id := films[0]["Id"].(string)
	name := films[0]["Name"].(string)

	report := `{"ItemId":"` + id + `","PositionTicks":600000000,"PlaySessionId":"play-1"}`
	if rec := watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing/Progress", report); rec.Code != http.StatusNoContent {
		t.Fatalf("reporting progress = %d", rec.Code)
	}

	// Open, and already in the log rather than only once it ends.
	watches, total, err := db.History(t.Context(), store.HistoryFilter{}, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("one stream should be one row, got %d", total)
	}
	if watches[0].Complete {
		t.Error("the stream has not stopped, so the row is not complete")
	}
	if watches[0].Title != name || watches[0].Surface != "jellyfin" || watches[0].Device != "television" {
		t.Errorf("the row lost what it was about: %+v", watches[0])
	}
	if watches[0].Name == "" || watches[0].UserID == 0 {
		t.Errorf("the row should name the viewer: %+v", watches[0])
	}
	if watches[0].Position != 60*time.Second {
		t.Errorf("position = %s, want 1m", watches[0].Position)
	}

	// Paused, then the same report again: one pause, not one every ten
	// seconds, and the file request in between must not undo it.
	paused := `{"ItemId":"` + id + `","PositionTicks":900000000,"IsPaused":true,"PlaySessionId":"play-1"}`
	watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing/Progress", paused)
	watching(t, server, token, "television", http.MethodGet, "/Videos/"+id+"/stream", "")
	watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing/Progress", paused)

	stopped := `{"ItemId":"` + id + `","PositionTicks":900000000,"PlaySessionId":"play-1"}`
	if rec := watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing/Stopped", stopped); rec.Code != http.StatusNoContent {
		t.Fatalf("reporting a stop = %d", rec.Code)
	}

	watches, total, err = db.History(t.Context(), store.HistoryFilter{}, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("one stream is still one row, got %d", total)
	}
	closed := watches[0]
	if !closed.Complete {
		t.Error("the client said it had stopped, so the row should be complete")
	}
	if closed.Position != 90*time.Second {
		t.Errorf("position = %s, want 1m30s", closed.Position)
	}
	if closed.Ended.Before(closed.Started) {
		t.Errorf("the row ends before it starts: %s to %s", closed.Started, closed.Ended)
	}
	if closed.Watched() > closed.Ended.Sub(closed.Started) {
		t.Errorf("watched (%s) is longer than the stream was open (%s)",
			closed.Watched(), closed.Ended.Sub(closed.Started))
	}

	// And the totals can see it.
	viewers, err := db.WatchersSince(t.Context(), closed.Started.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(viewers) != 1 || viewers[0].Plays != 1 {
		t.Errorf("the totals say %+v", viewers)
	}
}

// A stream an administrator stopped records why. The register's reason is the
// one thing the log can say that the status page cannot once it is gone.
func TestATerminatedStreamIsRecordedWithItsReason(t *testing.T) {
	server, token, db := library(t)

	recorder := history.New(history.Options{DB: db})
	register := nowplaying.New(nowplaying.Options{Changes: recorder.Record})
	server.Playing = register

	c := call{t: t, server: server, token: token}
	films := c.items("/Users/" + c.me() + "/Items?ParentId=" + c.views()["Films"]["Id"].(string))
	id := films[0]["Id"].(string)

	report := `{"ItemId":"` + id + `","PositionTicks":600000000,"PlaySessionId":"play-1"}`
	watching(t, server, token, "television", http.MethodPost, "/Sessions/Playing/Progress", report)

	live := register.List()
	if len(live) != 1 {
		t.Fatalf("expected one stream, got %+v", live)
	}
	if _, ok := register.Terminate(live[0].Key, "Bedtime."); !ok {
		t.Fatal("the stream could not be terminated")
	}

	watches, total, err := db.History(t.Context(), store.HistoryFilter{}, store.Page{Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("one stream should be one row, got %d", total)
	}
	if watches[0].Reason != "Bedtime." {
		t.Errorf("the log does not say why it ended: %q", watches[0].Reason)
	}
	if !watches[0].Complete {
		t.Error("a stream somebody stopped did end, so the row is complete")
	}
}
