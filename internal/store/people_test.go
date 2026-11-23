package store_test

import (
	"slices"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
	"github.com/inknironapps/bask-media-server/internal/store"
)

func credit(name string, role domain.CreditRole, part string, position int) store.Credit {
	return store.Credit{
		Credit:   domain.Credit{Name: name, Role: role, Part: part},
		Position: position,
	}
}

func TestCreditsComeBackInTheOrderAListIsRead(t *testing.T) {
	s := newShelf(t)

	if err := s.db.SetCredits(s.ctx, s.filmA, []store.Credit{
		credit("Amy Adams", domain.CreditActor, "Louise Banks", 0),
		credit("Jeremy Renner", domain.CreditActor, "Ian Donnelly", 1),
		credit("Denis Villeneuve", domain.CreditDirector, "", 0),
		credit("Eric Heisserer", domain.CreditWriter, "", 0),
	}); err != nil {
		t.Fatal(err)
	}

	item, err := s.db.Item(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.People) != 4 {
		t.Fatalf("people = %+v", item.People)
	}

	// The cast in the provider's own billing order, which is the only thing
	// that makes the first names the ones worth showing.
	var cast []string
	for _, person := range item.People {
		if person.Role == domain.CreditActor {
			cast = append(cast, person.Name)
		}
	}
	if !slices.Equal(cast, []string{"Amy Adams", "Jeremy Renner"}) {
		t.Fatalf("cast = %v", cast)
	}

	// And the part is on the credit, not the person: the same actor plays
	// different parts in different films.
	for _, person := range item.People {
		if person.Name == "Amy Adams" && person.Part != "Louise Banks" {
			t.Fatalf("part = %q", person.Part)
		}
		if person.Role == domain.CreditDirector && person.Part != "" {
			t.Fatalf("a director has a part: %+v", person)
		}
	}
}

func TestOnePersonInTwoRolesIsBothAndOneRow(t *testing.T) {
	s := newShelf(t)

	// Writing and directing the same film is two credits for one person.
	if err := s.db.SetCredits(s.ctx, s.filmA, []store.Credit{
		credit("Terry Gilliam", domain.CreditDirector, "", 0),
		credit("Terry Gilliam", domain.CreditWriter, "", 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetCredits(s.ctx, s.filmB, []store.Credit{
		credit("Terry Gilliam", domain.CreditDirector, "", 0),
	}); err != nil {
		t.Fatal(err)
	}

	credits, err := s.db.Credits(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if len(credits) != 2 {
		t.Fatalf("credits = %+v", credits)
	}

	// One row in the people table, shared: an actor in forty films is one
	// row, and "what else are they in" is an index lookup.
	directors, err := s.db.PeopleNames(s.ctx, domain.CreditDirector)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(directors, []string{"Terry Gilliam"}) {
		t.Fatalf("directors = %v", directors)
	}
	counts, err := s.db.SectionPeople(s.ctx, s.films, string(domain.KindMovie), domain.CreditDirector)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 1 || counts[0].Held != 2 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestSettingCreditsReplacesThemWhole(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetCredits(s.ctx, s.filmA, []store.Credit{
		credit("Amy Adams", domain.CreditActor, "Louise", 0),
		credit("Someone Else", domain.CreditActor, "Extra", 1),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetCredits(s.ctx, s.filmA, []store.Credit{
		credit("Amy Adams", domain.CreditActor, "Louise Banks", 0),
	}); err != nil {
		t.Fatal(err)
	}

	credits, err := s.db.Credits(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if len(credits) != 1 {
		t.Fatalf("credits = %+v", credits)
	}
	// And the part was corrected, not duplicated.
	if credits[0].Part != "Louise Banks" {
		t.Fatalf("part = %q", credits[0].Part)
	}
}

func TestCreditsAreCappedAndCleaned(t *testing.T) {
	s := newShelf(t)

	var many []store.Credit
	for i := range 80 {
		many = append(many, credit("Actor "+string(rune('A'+i%26))+string(rune('a'+i/26)),
			domain.CreditActor, "", i))
	}
	// And some that cannot be stored: no name, a role nobody has, and a
	// repeat of one already in the list.
	many = append(many,
		credit("   ", domain.CreditActor, "", 0),
		credit("Someone", domain.CreditRole("composer"), "", 0),
		credit("Actor Aa", domain.CreditActor, "", 0),
	)
	if err := s.db.SetCredits(s.ctx, s.filmA, many); err != nil {
		t.Fatal(err)
	}

	credits, err := s.db.Credits(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if len(credits) != 40 {
		t.Fatalf("kept %d credits, want the cap of 40", len(credits))
	}
	for _, one := range credits {
		if one.Role == domain.CreditRole("composer") {
			t.Fatal("a role this server does not keep was stored")
		}
	}
}

func TestAPhotographIsKeptOnceAndNeverCleared(t *testing.T) {
	s := newShelf(t)

	withPicture := credit("Amy Adams", domain.CreditActor, "Louise", 0)
	withPicture.Thumb = "a-cache-key"
	if err := s.db.SetCredits(s.ctx, s.filmA, []store.Credit{withPicture}); err != nil {
		t.Fatal(err)
	}

	// Another film, same actor, no picture this time: the picture somebody
	// already has is better than no picture.
	if err := s.db.SetCredits(s.ctx, s.filmB, []store.Credit{
		credit("Amy Adams", domain.CreditActor, "Somebody", 0),
	}); err != nil {
		t.Fatal(err)
	}

	credits, err := s.db.Credits(s.ctx, s.filmB)
	if err != nil {
		t.Fatal(err)
	}
	if len(credits) != 1 || credits[0].Thumb != "a-cache-key" {
		t.Fatalf("credit = %+v", credits[0])
	}
}

func TestNarrowingALibraryByWhoWorkedOnIt(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetCredits(s.ctx, s.filmA, []store.Credit{
		credit("Denis Villeneuve", domain.CreditDirector, "", 0),
		credit("Amy Adams", domain.CreditActor, "Louise", 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetCredits(s.ctx, s.filmB, []store.Credit{
		credit("Terry Gilliam", domain.CreditDirector, "", 0),
	}); err != nil {
		t.Fatal(err)
	}

	titles := func(page store.Page) []string {
		t.Helper()
		items, _, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie, page)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.Title)
		}
		return out
	}

	byDirector := store.Page{Size: 50, People: []store.CreditFilter{{
		Roles: []domain.CreditRole{domain.CreditDirector},
		Names: []string{"Denis Villeneuve"},
	}}}
	if got := titles(byDirector); !slices.Equal(got, []string{"Arrival"}) {
		t.Fatalf("by director = %v", got)
	}

	// A name in the wrong role matches nothing, which is the point of the
	// role being part of the question.
	wrongRole := store.Page{Size: 50, People: []store.CreditFilter{{
		Roles: []domain.CreditRole{domain.CreditActor},
		Names: []string{"Denis Villeneuve"},
	}}}
	if got := titles(wrongRole); len(got) != 0 {
		t.Fatalf("director asked for as an actor = %v", got)
	}

	// One filter over every role is "anything this person worked on", which
	// is what a wire that does not name the role is asking.
	anyRole := store.Page{Size: 50, People: []store.CreditFilter{{
		Roles: domain.CreditRoles(),
		Names: []string{"Denis Villeneuve", "Terry Gilliam"},
	}}}
	if got := titles(anyRole); !slices.Equal(got, []string{"Arrival", "Brazil"}) {
		t.Fatalf("any role = %v", got)
	}
}

func TestAStudioIsRecordedAndNarrowedBy(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetStudio(s.ctx, s.filmA, "  Paramount  Pictures "); err != nil {
		t.Fatal(err)
	}

	item, err := s.db.Item(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if item.Studio != "Paramount Pictures" {
		t.Fatalf("studio = %q", item.Studio)
	}

	// An empty answer does not erase one: the same rule as the genres.
	if err := s.db.SetStudio(s.ctx, s.filmA, ""); err != nil {
		t.Fatal(err)
	}
	item, err = s.db.Item(s.ctx, s.filmA)
	if err != nil {
		t.Fatal(err)
	}
	if item.Studio != "Paramount Pictures" {
		t.Fatalf("an empty answer cleared the studio: %q", item.Studio)
	}

	items, total, err := s.db.SectionItems(s.ctx, s.films, domain.KindMovie,
		store.Page{Size: 50, Studio: "Paramount Pictures"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 || items[0].Title != "Arrival" {
		t.Fatalf("by studio = %v", items)
	}

	studios, err := s.db.StudioNames(s.ctx, s.films, string(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	if len(studios) != 1 || studios[0].Name != "Paramount Pictures" || studios[0].Held != 1 {
		t.Fatalf("studios = %+v", studios)
	}
}

func TestASmartCollectionCanAskWhoMadeIt(t *testing.T) {
	s := newShelf(t)
	if err := s.db.SetCredits(s.ctx, s.filmA, []store.Credit{
		credit("Denis Villeneuve", domain.CreditDirector, "", 0),
		credit("Amy Adams", domain.CreditActor, "Louise", 0),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.SetStudio(s.ctx, s.filmB, "Universal"); err != nil {
		t.Fatal(err)
	}

	titles := func(rules domain.Smart) []string {
		t.Helper()
		items, held, err := s.db.SmartPreview(s.ctx, rules, store.Page{Size: 50})
		if err != nil {
			t.Fatal(err)
		}
		if held != len(items) {
			t.Fatalf("held %d but %d came back", held, len(items))
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.Title)
		}
		return out
	}

	if got := titles(domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartDirector, Op: domain.SmartIs, Value: "Denis Villeneuve"},
	}}); !slices.Equal(got, []string{"Arrival"}) {
		t.Fatalf("by director = %v", got)
	}
	if got := titles(domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartActor, Op: domain.SmartIs, Value: "Amy Adams"},
	}}); !slices.Equal(got, []string{"Arrival"}) {
		t.Fatalf("by actor = %v", got)
	}
	if got := titles(domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartStudio, Op: domain.SmartIs, Value: "Universal"},
	}}); !slices.Equal(got, []string{"Brazil"}) {
		t.Fatalf("by studio = %v", got)
	}
	// A director asked for as an actor matches nothing, because the role is
	// part of the question.
	if got := titles(domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartActor, Op: domain.SmartIs, Value: "Denis Villeneuve"},
	}}); len(got) != 0 {
		t.Fatalf("director as actor = %v", got)
	}

	// And a rule needs a name.
	if err := store.ValidateSmart(domain.Smart{Rules: []domain.SmartRule{
		{Field: domain.SmartDirector, Op: domain.SmartIs},
	}}); err == nil {
		t.Fatal("a director rule with no name was accepted")
	}
}
