package gchat

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	people "google.golang.org/api/people/v1"

	"comms/internal/naming"
)

type fakePeopleAPI struct {
	mu      sync.Mutex
	names   map[string]string
	err     error
	batches [][]string
}

func (f *fakePeopleAPI) probe(context.Context) error { return f.err }

func (f *fakePeopleAPI) lookupNames(_ context.Context, ids []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, append([]string(nil), ids...))
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		out[id] = f.names[id]
	}
	return out, nil
}

func TestPreferredPersonName(t *testing.T) {
	names := []*people.Name{
		{DisplayName: "Zulu"},
		{DisplayName: "Source", Metadata: &people.FieldMetadata{SourcePrimary: true}},
		{DisplayName: "Primary", Metadata: &people.FieldMetadata{Primary: true}},
		{DisplayName: "Alpha"},
	}
	if got := preferredPersonName(names); got != "Primary" {
		t.Fatalf("preferredPersonName = %q, want Primary", got)
	}
	if got := preferredPersonName([]*people.Name{{DisplayName: "Zulu"}, {DisplayName: "Alpha"}}); got != "Alpha" {
		t.Fatalf("lexical fallback = %q, want Alpha", got)
	}
}

func TestPeopleNamesEnrichArchive(t *testing.T) {
	chatAPI := newFakeAPI()
	seedFake(chatAPI)
	// User-authenticated Chat commonly gives only users/{id}; exercise that
	// exact shape instead of relying on the membership display name fallback.
	chatAPI.members = nil
	peopleAPI := &fakePeopleAPI{names: map[string]string{"users/1": "Jane Doe"}}
	acct := testAcct()
	acct.ResolveChatNames = true
	c, db, w := newTestConnector(t, chatAPI, acct, withPeopleAPI(peopleAPI))

	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if name, ok, err := db.GetChatPersonName(testSrc, "users/1"); err != nil || !ok || name != "Jane Doe" {
		t.Fatalf("cached People name = %q, %v, %v", name, ok, err)
	}
	peopleAPI.mu.Lock()
	calls := len(peopleAPI.batches)
	peopleAPI.mu.Unlock()
	if calls != 1 {
		t.Fatalf("People batches = %d, want one", calls)
	}
	stem := naming.ChatStem(testTag, "space", "team-platform", naming.Hash8("spaces/AAA"))
	day := readFile(t, w.Root, "2026/08/06/"+stem+".md")
	if !bytes.Contains(day, []byte("**Jane Doe**")) || bytes.Contains(day, []byte("**users/1**")) {
		t.Fatalf("archive did not use the enriched display name:\n%s", day)
	}
	if !bytes.Contains(day, []byte(" ^gchat-u1-")) {
		t.Fatalf("archive lacks a stable message block id:\n%s", day)
	}
}

func TestPeopleFailureIsBestEffort(t *testing.T) {
	chatAPI := newFakeAPI()
	seedFake(chatAPI)
	chatAPI.members = nil
	acct := testAcct()
	acct.ResolveChatNames = true
	c, _, w := newTestConnector(t, chatAPI, acct, withPeopleAPI(&fakePeopleAPI{err: errors.New("People API disabled")}))
	c.retryOpts.MaxAttempts = 1

	if err := c.Sync(context.Background()); err != nil {
		t.Fatalf("People failure stopped Chat archival: %v", err)
	}
	stem := naming.ChatStem(testTag, "space", "team-platform", naming.Hash8("spaces/AAA"))
	day := readFile(t, w.Root, "2026/08/06/"+stem+".md")
	if !bytes.Contains(day, []byte("**users/1**")) {
		t.Fatalf("opaque fallback missing after People failure:\n%s", day)
	}
}
