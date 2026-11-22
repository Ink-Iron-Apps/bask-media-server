package jellyfin_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/inknironapps/bask-media-server/internal/domain"
)

// A smart collection is a collection to a media client: the same shelf, the
// same folder, the same way in. Nothing about the Jellyfin surface knows one
// kind from the other, which is the point of putting the rules in the store.
func TestASmartCollectionIsJustACollectionToAClient(t *testing.T) {
	server, token, db := library(t)
	ctx := context.Background()
	c := call{t: t, server: server, token: token}

	made, err := db.CreateCollection(ctx, "Films on this server", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetCollectionRules(ctx, made.ID, &domain.Smart{
		Rules: []domain.SmartRule{{Field: domain.SmartKind, Op: domain.SmartIs, Value: string(domain.KindMovie)}},
	}); err != nil {
		t.Fatal(err)
	}

	listed := c.items("/Items?IncludeItemTypes=BoxSet&Recursive=true")
	if len(listed) != 1 || listed[0]["Name"] != "Films on this server" {
		t.Fatalf("collections = %v", listed)
	}
	if listed[0]["IsFolder"] != true {
		t.Fatal("a collection that is not a folder cannot be opened")
	}

	inside := c.items("/Items?ParentId=" + listed[0]["Id"].(string))
	if len(inside) != 1 || inside[0]["Name"] != "Arrival" {
		t.Fatalf("inside the collection: %v", inside)
	}
}

// And the library restriction still decides: a device that may see only the
// music library is not shown a collection of films, even one that fills
// itself. An empty shelf announces that something exists.
func TestASmartCollectionOfHiddenFilmsIsNotOffered(t *testing.T) {
	server, token, db := library(t)
	ctx := context.Background()
	c := call{t: t, server: server, token: token}

	made, err := db.CreateCollection(ctx, "Films only", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetCollectionRules(ctx, made.ID, &domain.Smart{
		Rules: []domain.SmartRule{{Field: domain.SmartKind, Op: domain.SmartIs, Value: string(domain.KindMovie)}},
	}); err != nil {
		t.Fatal(err)
	}

	narrow(t, db, "Music")

	if listed := c.items("/Items?IncludeItemTypes=BoxSet&Recursive=true"); len(listed) != 0 {
		t.Fatalf("a music-only device was shown %v", listed)
	}
	res, _ := c.do(http.MethodGet, "/Users/"+c.me()+"/Items/"+collectionIDForTest(made.ID), nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("reading it directly gave %d", res.StatusCode)
	}
}
