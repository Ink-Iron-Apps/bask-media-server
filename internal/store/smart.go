package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// A smart collection is a question, not a list.
//
// A hand-picked collection stores membership: these four films, in this
// order. A smart one stores the rules and asks them of the library every
// time somebody opens it, which is the whole reason to have it — a film that
// arrives tonight is in "Unwatched science fiction" tonight, with nobody
// remembering to put it there.
//
// Three decisions shape this file.
//
// The rules are typed, never SQL. What is stored is a field, an operator and
// a value, and this file is the only thing that turns them into a statement —
// always with placeholders, never with the value pasted in. A stored string
// that reaches the database as SQL is the oldest hole there is, and a server
// whose administration page is behind a token is not a reason to dig it.
//
// A field the server does not know is refused when it is saved, not ignored
// when it is read. A rule that silently does nothing is worse than one that
// will not save: the collection looks right and holds the wrong films.
//
// Watch state is the asking account's. "Unwatched" cannot mean anything else,
// so a smart collection with a watch rule genuinely holds different films for
// different people — which is a property of the question, not a bug in the
// answer. A handle with no account behind it has watched nothing; see
// [DB.Viewer].

// smartKind is what sort of value a field takes, so an editor knows which
// input to put in front of somebody.
type smartKind string

const (
	smartValueLibrary smartKind = "library" // a library id
	smartValueChoice  smartKind = "choice"  // one of a fixed list
	smartValueText    smartKind = "text"

	// smartValueGenre is a word, like text, but one of a list the library
	// decides rather than one this file can name: the editor is handed the
	// genres actually in use, and a rule may still carry a genre nothing
	// holds yet — a film tagged tomorrow joins the shelf by itself, which is
	// the whole point of a collection that fills itself.
	smartValueGenre smartKind = "genre"

	smartValueNumber  smartKind = "number"
	smartValueDays    smartKind = "days"
	smartValueMinutes smartKind = "minutes"
	smartValueNone    smartKind = "none" // the operator is the whole rule
)

// smartSpec is one field: what it takes, how it may be compared, and how each
// comparison becomes SQL.
type smartSpec struct {
	label   string
	value   smartKind
	ops     []domain.SmartOp
	choices []string

	// compile turns one rule into a fragment and its arguments. The fragment
	// refers to the item as `i`, which is what every caller aliases it.
	compile func(op domain.SmartOp, value string, viewer int64) (string, []any)
}

// collectable is what a collection can hold, smart or otherwise: the things a
// client shows on a shelf. A season or a track in a collection would be a
// grouping nobody asked for, and the hand-picked editor already refuses them.
var collectable = []string{
	string(domain.KindMovie), string(domain.KindShow),
	string(domain.KindAlbum), string(domain.KindArtist),
	string(domain.KindPhotoAlbum),
}

// smartFamily matches an item and everything under it, as a correlated
// predicate over `items d`.
//
// Two levels because that is as deep as the library goes: a show has seasons
// which have episodes, an artist has albums which have tracks. It is what
// makes a rule about files work on a show at all — a show has no files, its
// episodes do, and "has a 4K file" has to mean "somewhere in it".
const smartFamily = `(d.id = i.id OR d.parent_id = i.id
	OR d.parent_id IN (SELECT m.id FROM items m WHERE m.parent_id = i.id))`

// smartPlayable counts the things in or under an item that can actually be
// played: an episode counts, a season does not, a trailer does not.
const smartPlayable = `(SELECT COUNT(*) FROM items d
	WHERE ` + smartFamily + ` AND d.extra = '' AND d.duration_ms > 0)`

// smartSeen counts how many of those one account has finished.
const smartSeen = `(SELECT COUNT(*) FROM items d
	WHERE ` + smartFamily + ` AND d.extra = '' AND d.duration_ms > 0
	  AND EXISTS (SELECT 1 FROM progress pr
	               WHERE pr.item_id = d.id AND pr.user_id = ? AND pr.watched <> 0))`

// smartFields is the whole vocabulary. Everything else in this file reads it,
// including the validation that runs before a rule set is stored and the
// listing the editor builds its menus from, so a new field is one entry here
// rather than four edits in three packages.
var smartFields = map[domain.SmartField]smartSpec{
	domain.SmartLibrary: {
		label: "Library",
		value: smartValueLibrary,
		ops:   []domain.SmartOp{domain.SmartIs, domain.SmartIsNot},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			id, _ := strconv.ParseInt(value, 10, 64)
			if op == domain.SmartIsNot {
				return `i.section_id <> ?`, []any{id}
			}
			return `i.section_id = ?`, []any{id}
		},
	},
	domain.SmartKind: {
		label:   "Kind",
		value:   smartValueChoice,
		ops:     []domain.SmartOp{domain.SmartIs, domain.SmartIsNot},
		choices: collectable,
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			if op == domain.SmartIsNot {
				return `i.kind <> ?`, []any{value}
			}
			return `i.kind = ?`, []any{value}
		},
	},
	domain.SmartTitle: {
		label: "Title",
		value: smartValueText,
		ops:   []domain.SmartOp{domain.SmartContains, domain.SmartIs, domain.SmartStarts, domain.SmartIsNot},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			return matchText("i.title", op, value)
		},
	},
	domain.SmartSummary: {
		label: "Summary",
		value: smartValueText,
		ops:   []domain.SmartOp{domain.SmartContains},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			return matchText("i.summary", op, value)
		},
	},
	domain.SmartPerformer: {
		label: "Performer",
		value: smartValueText,
		ops:   []domain.SmartOp{domain.SmartContains, domain.SmartIs},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			return matchText("i.performer", op, value)
		},
	},
	domain.SmartGenre: {
		label: "Genre",
		value: smartValueGenre,
		ops:   []domain.SmartOp{domain.SmartIs, domain.SmartIsNot},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			// Correlated rather than joined: a film in two genres would
			// otherwise be two rows, and a collection would hold it twice.
			//
			// The item's own genres, not its children's — see the note at
			// the top of genres.go on why genres are not inherited. Every
			// kind a collection can hold carries its own.
			carries := `EXISTS (SELECT 1 FROM item_genres ig
			              JOIN genres g ON g.id = ig.genre_id
			             WHERE ig.item_id = i.id AND g.name = ?)`
			if op == domain.SmartIsNot {
				return `NOT ` + carries, []any{value}
			}
			return carries, []any{value}
		},
	},
	domain.SmartYear: {
		label: "Year",
		value: smartValueNumber,
		ops: []domain.SmartOp{domain.SmartIs, domain.SmartIsNot, domain.SmartBefore,
			domain.SmartAfter, domain.SmartAtLeast, domain.SmartAtMost},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			// A year of zero is a year nobody filled in, so every comparison
			// leaves those out rather than filing them under the first
			// century: "made before 1980" must not mean "or we have no idea".
			year, _ := strconv.Atoi(value)
			return `i.year > 0 AND ` + compare("i.year", op), []any{year}
		},
	},
	domain.SmartCertificate: {
		label: "Certificate",
		value: smartValueText,
		ops:   []domain.SmartOp{domain.SmartIs, domain.SmartIsNot},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			// An empty value is allowed here and means "nothing rated it",
			// which is the one certificate somebody actually wants to search
			// for and the one they cannot type.
			if op == domain.SmartIsNot {
				return `i.rating <> ?`, []any{value}
			}
			return `i.rating = ?`, []any{value}
		},
	},
	domain.SmartAge: {
		label: "Age rating",
		value: smartValueNumber,
		ops:   []domain.SmartOp{domain.SmartAtMost, domain.SmartAtLeast},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			age, _ := strconv.Atoi(value)
			// Unknown is stored as -1, and a ceiling must not sweep it up:
			// "suitable for 12 and under" cannot include the films nobody has
			// rated, which is the same rule the account ceilings follow.
			return `i.rating_age >= 0 AND ` + compare("i.rating_age", op), []any{age}
		},
	},
	domain.SmartAdded: {
		label: "Added",
		value: smartValueDays,
		ops:   []domain.SmartOp{domain.SmartWithin, domain.SmartBefore},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			days, _ := strconv.Atoi(value)
			cutoff := millis(time.Now().UTC().AddDate(0, 0, -days))
			if op == domain.SmartBefore {
				return `i.added_at < ?`, []any{cutoff}
			}
			return `i.added_at >= ?`, []any{cutoff}
		},
	},
	domain.SmartDuration: {
		label: "Length",
		value: smartValueMinutes,
		ops:   []domain.SmartOp{domain.SmartAtLeast, domain.SmartAtMost},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			minutes, _ := strconv.Atoi(value)
			ms := int64(minutes) * 60 * 1000
			// Length is the item's own, so this is a rule about films,
			// episodes and tracks. A show has no length of its own and is
			// left out rather than counted as nought minutes.
			return `i.duration_ms > 0 AND ` + compare("i.duration_ms", op), []any{ms}
		},
	},
	domain.SmartResolution: {
		label: "Height in pixels",
		value: smartValueNumber,
		ops:   []domain.SmartOp{domain.SmartAtLeast, domain.SmartAtMost},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			height, _ := strconv.Atoi(value)
			return existsInFamily(`p.height > 0 AND ` + compare("p.height", op)), []any{height}
		},
	},
	domain.SmartContainer: {
		label: "Container",
		value: smartValueText,
		ops:   []domain.SmartOp{domain.SmartIs},
		compile: func(_ domain.SmartOp, value string, _ int64) (string, []any) {
			return existsInFamily(`LOWER(p.container) = LOWER(?)`), []any{value}
		},
	},
	domain.SmartEdition: {
		label: "Edition",
		value: smartValueText,
		ops:   []domain.SmartOp{domain.SmartIs, domain.SmartContains},
		compile: func(op domain.SmartOp, value string, _ int64) (string, []any) {
			if op == domain.SmartContains {
				return existsInFamily(`p.edition LIKE ? ESCAPE '\'`), []any{"%" + escapeLike(value) + "%"}
			}
			return existsInFamily(`p.edition = ?`), []any{value}
		},
	},
	domain.SmartWatched: {
		label: "Watched",
		value: smartValueNone,
		ops:   []domain.SmartOp{domain.SmartNo, domain.SmartYes, domain.SmartPartly},
		compile: func(op domain.SmartOp, _ string, viewer int64) (string, []any) {
			switch op {
			case domain.SmartYes:
				return `(` + smartPlayable + ` > 0 AND ` + smartSeen + ` = ` + smartPlayable + `)`,
					[]any{viewer}
			case domain.SmartPartly:
				return `(` + smartSeen + ` > 0 AND ` + smartSeen + ` < ` + smartPlayable + `)`,
					[]any{viewer, viewer}
			default:
				return `(` + smartSeen + ` = 0)`, []any{viewer}
			}
		},
	},
}

// matchText is the one place a text comparison is built, so every field that
// takes words behaves the same way.
func matchText(column string, op domain.SmartOp, value string) (string, []any) {
	switch op {
	case domain.SmartIs:
		return column + ` = ?`, []any{value}
	case domain.SmartIsNot:
		return column + ` <> ?`, []any{value}
	case domain.SmartStarts:
		return column + ` LIKE ? ESCAPE '\'`, []any{escapeLike(value) + "%"}
	default:
		return column + ` LIKE ? ESCAPE '\'`, []any{"%" + escapeLike(value) + "%"}
	}
}

// escapeLike neutralises the wildcards in a value somebody typed, because a
// title containing a per-cent sign is a title, not a pattern.
func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "%", `\%`)
	return strings.ReplaceAll(value, "_", `\_`)
}

// compare maps a numeric operator onto a column.
func compare(column string, op domain.SmartOp) string {
	switch op {
	case domain.SmartIsNot:
		return column + ` <> ?`
	case domain.SmartBefore:
		return column + ` < ?`
	case domain.SmartAtMost:
		return column + ` <= ?`
	case domain.SmartAfter:
		return column + ` > ?`
	case domain.SmartAtLeast:
		return column + ` >= ?`
	default:
		return column + ` = ?`
	}
}

// existsInFamily asks something of the files in or under an item.
func existsInFamily(predicate string) string {
	return `EXISTS (SELECT 1 FROM items d JOIN parts p ON p.item_id = d.id
	                 WHERE ` + smartFamily + ` AND ` + predicate + `)`
}

// smartRuleLimit caps how many conditions one collection may carry. Generous
// for anything a person would type and short of the query that takes a second
// to plan.
const smartRuleLimit = 24

// ValidateSmart checks a rule set before it is stored, and says in plain words
// what is wrong with it.
//
// Before it is stored rather than when it is read: a rule the server cannot
// compile is a collection that quietly holds the wrong films, and the only
// person who can fix it is the one who is typing it right now.
func ValidateSmart(s domain.Smart) error {
	switch s.Match {
	case "", domain.SmartAll, domain.SmartAny:
	default:
		return fmt.Errorf("a rule set matches %q or %q, not %q", domain.SmartAll, domain.SmartAny, s.Match)
	}
	switch s.Sort {
	case "", domain.SmartByTitle, domain.SmartByYear, domain.SmartByAdded, domain.SmartByDuration:
	default:
		return fmt.Errorf("%q is not an order this server can sort by", s.Sort)
	}
	if s.Limit < 0 {
		return fmt.Errorf("a limit cannot be negative")
	}
	if len(s.Rules) > smartRuleLimit {
		return fmt.Errorf("a collection may carry %d rules, not %d", smartRuleLimit, len(s.Rules))
	}
	for _, rule := range s.Rules {
		if err := validateRule(rule); err != nil {
			return err
		}
	}
	return nil
}

func validateRule(rule domain.SmartRule) error {
	spec, known := smartFields[rule.Field]
	if !known {
		if rule.Field == "" {
			return fmt.Errorf("a rule needs something to look at")
		}
		return fmt.Errorf("this server knows nothing about %q", rule.Field)
	}
	allowed := false
	for _, op := range spec.ops {
		if op == rule.Op {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("%s cannot be compared with %q", strings.ToLower(spec.label), rule.Op)
	}

	value := strings.TrimSpace(rule.Value)
	switch spec.value {
	case smartValueNone:
		return nil
	case smartValueLibrary:
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("%q is not a library", rule.Value)
		}
	case smartValueChoice:
		for _, choice := range spec.choices {
			if choice == value {
				return nil
			}
		}
		return fmt.Errorf("%q is not one of %s", rule.Value, strings.Join(spec.choices, ", "))
	case smartValueNumber, smartValueDays, smartValueMinutes:
		number, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%q is not a number", rule.Value)
		}
		if number < 0 {
			return fmt.Errorf("%q cannot be negative", rule.Value)
		}
	case smartValueText:
		// The certificate is the one text field whose empty value means
		// something: the films nobody rated.
		if value == "" && rule.Field != domain.SmartCertificate {
			return fmt.Errorf("%s needs something to compare against", strings.ToLower(spec.label))
		}
	case smartValueGenre:
		// Not checked against the genres in the library on purpose. A rule
		// is a question asked of the library every time it is read, and a
		// library that has no science fiction today may have some tomorrow:
		// refusing the rule would be refusing the empty shelf that fills
		// itself later.
		if value == "" {
			return fmt.Errorf("a genre rule needs a genre")
		}
		if len(value) > maxGenreLength {
			return fmt.Errorf("%q is too long to be a genre", rule.Value)
		}
	}
	return nil
}

// compiledSmart is a rule set turned into the parts of a statement.
type compiledSmart struct {
	where string
	args  []any
	order string
	limit int
}

// compileSmart builds the query behind one smart collection.
//
// Invalid rules are dropped here rather than refused, because this runs on
// what is already stored: a field this build of the server no longer knows
// should cost one condition, not a listing. Everything that reaches storage
// has been through [ValidateSmart], so the only way to get here with a bad
// rule is a server that changed its mind.
//
// A rule set that ends with no usable conditions matches the whole library,
// which is deliberate: "everything, newest first, the first twenty" is a
// collection somebody wants.
func compileSmart(s domain.Smart, viewer int64) compiledSmart {
	var (
		fragments []string
		args      []any
	)
	for _, rule := range s.Rules {
		spec, known := smartFields[rule.Field]
		if !known || validateRule(rule) != nil {
			continue
		}
		fragment, values := spec.compile(rule.Op, strings.TrimSpace(rule.Value), viewer)
		fragments = append(fragments, "("+fragment+")")
		args = append(args, values...)
	}

	where := ""
	if len(fragments) > 0 {
		joiner := " AND "
		if s.Match == domain.SmartAny {
			joiner = " OR "
		}
		where = " AND (" + strings.Join(fragments, joiner) + ")"
	}
	return compiledSmart{where: where, args: args, order: smartOrder(s), limit: s.Limit}
}

// smartOrder is the ORDER BY, with the item's id last so a page boundary never
// falls in the middle of a tie — two films of the same year in the same order
// every time, or a client paging a collection sees one twice and another not
// at all.
func smartOrder(s domain.Smart) string {
	var columns []string
	switch s.Sort {
	case domain.SmartByYear:
		columns = []string{"i.year", "i.sort_title", "i.id"}
	case domain.SmartByAdded:
		columns = []string{"i.added_at", "i.id"}
	case domain.SmartByDuration:
		columns = []string{"i.duration_ms", "i.sort_title", "i.id"}
	default:
		columns = []string{"i.sort_title", "i.year", "i.id"}
	}
	if s.Desc {
		for i := range columns {
			columns[i] += " DESC"
		}
	}
	return strings.Join(columns, ", ")
}

// smartBase is the part of the query that is true of every smart collection:
// the things a collection may hold, and nothing that is an extra.
//
// No restriction on what is above an item, because an album sits under its
// artist and a photograph album sits under the folder that holds it: the kind
// is what decides whether a thing belongs on a shelf, not its depth.
const smartBase = `FROM items i
	WHERE i.extra = '' AND i.kind IN ('` + kindList + `')`

// kindList is collectable, spelled for the SQL above. Built as a constant
// rather than from the slice so the statement stays a constant string.
const kindList = string(domain.KindMovie) + `','` + string(domain.KindShow) +
	`','` + string(domain.KindAlbum) + `','` + string(domain.KindArtist) +
	`','` + string(domain.KindPhotoAlbum)

// SmartFieldInfo describes one field for an editor: what to call it, what it
// takes, and how it may be compared.
//
// The server owns this list because the server is what has to be able to
// answer the question. An editor that keeps its own copy agrees with it until
// somebody adds a field, and then offers a rule that will not save.
type SmartFieldInfo struct {
	Field   domain.SmartField `json:"field"`
	Label   string            `json:"label"`
	Value   string            `json:"value"`
	Ops     []domain.SmartOp  `json:"ops"`
	Choices []string          `json:"choices,omitempty"`
}

// SmartFields lists the vocabulary, in the order an editor should offer it.
func SmartFields() []SmartFieldInfo {
	order := []domain.SmartField{
		domain.SmartLibrary, domain.SmartKind, domain.SmartTitle, domain.SmartSummary,
		domain.SmartPerformer, domain.SmartGenre, domain.SmartYear, domain.SmartCertificate, domain.SmartAge,
		domain.SmartAdded, domain.SmartDuration, domain.SmartResolution,
		domain.SmartContainer, domain.SmartEdition, domain.SmartWatched,
	}
	out := make([]SmartFieldInfo, 0, len(order))
	for _, field := range order {
		spec := smartFields[field]
		out = append(out, SmartFieldInfo{
			Field: field, Label: spec.label, Value: string(spec.value),
			Ops: spec.ops, Choices: spec.choices,
		})
	}
	return out
}

// marshalSmart is how a rule set reaches the rules column: compact JSON, and
// the empty string for no rules at all, because empty is what tells every
// reader this is an ordinary hand-picked collection.
func marshalSmart(s *domain.Smart) (string, error) {
	if s == nil {
		return "", nil
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("store rules: %w", err)
	}
	return string(encoded), nil
}

// parseSmart reads the rules column back. A row that will not parse returns
// no rule set and no error: the caller marks the collection unreadable and
// shows it as holding nothing, which is the one behaviour that neither hides
// the fault nor takes the listing down with it.
func parseSmart(stored string) (*domain.Smart, bool) {
	if strings.TrimSpace(stored) == "" {
		return nil, true
	}
	var s domain.Smart
	if err := json.Unmarshal([]byte(stored), &s); err != nil {
		return nil, false
	}
	return &s, true
}

// SmartSorts lists the orders a smart collection may be shown in, for the same
// reason [SmartFields] exists: the server decides what it can sort by.
func SmartSorts() []domain.SmartSort {
	return []domain.SmartSort{
		domain.SmartByTitle, domain.SmartByYear, domain.SmartByAdded, domain.SmartByDuration,
	}
}

// SmartMatches lists the two ways a rule set can be read.
func SmartMatches() []domain.SmartMatch {
	return []domain.SmartMatch{domain.SmartAll, domain.SmartAny}
}

// SmartPreview is what a rule set currently matches, for an editor that wants
// to show somebody the answer before they save the question.
//
// The handle decides what is counted, so a preview run on an administrator's
// handle shows the whole library and the administrator's own watch state.
func (d *DB) SmartPreview(ctx context.Context, s domain.Smart, page Page) ([]domain.Item, int, error) {
	if err := ValidateSmart(s); err != nil {
		return nil, 0, err
	}
	return d.smartItems(ctx, s, page)
}
