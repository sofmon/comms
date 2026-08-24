package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanChatStems(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Two days of one space: the first (earliest day) hit wins.
	write("2026/08/06/gchat-work_group_team-platform_ab12cd34.md")
	write("2026/08/07/gchat-work_space_renamed-team_ab12cd34.md")
	// The SAME space archived by a second account: same hash, different tag,
	// its own frozen slug — it must be a separate entry, not a collision.
	write("2026/08/07/gchat-personal_space_platform-guild_ab12cd34.md")
	// A hyphenated label survives: the tag is everything before the first
	// underscore.
	write("2026/08/07/gchat-acme-corp_dm_jane-doe_deadbeef.md")
	// Other space types; a slug containing underscores keeps its tail hash.
	write("2026/08/07/gchat-work_dm_jane-doe_deadbeef.md")
	write("2026/08/07/gchat-work_space_a_deadbeef_12345678.md")
	// Must be ignored: email stems, attachment dirs, non-day paths, .tmp,
	// and the pre-multi-account stem shape (no label after "gchat").
	write("2026/08/07/143205_gmail-work_hello_ffffffff.md")
	write("2026/08/07/gchat-work_dm_jane-doe_deadbeef.d/120000_ab12cd34_f.pdf")
	write("notes/gchat-work_dm_stray_cccccccc.md")
	write(TempDirName + "/gchat-work_dm_litter_bbbbbbbb.md")
	write("2026/08/07/gchat-work_bot_odd-type_eeeeeeee.md") // unknown space type
	write("2026/08/07/gchat_dm_unlabelled_aaaaaaaa.md")     // no account label

	got, err := ScanChatStems(root)
	if err != nil {
		t.Fatalf("ScanChatStems: %v", err)
	}
	want := map[ChatStemKey]ChatStemInfo{
		{Tag: "gchat-work", Hash8: "ab12cd34"}:      {SpaceType: "group", Slug: "team-platform"}, // first hit wins
		{Tag: "gchat-personal", Hash8: "ab12cd34"}:  {SpaceType: "space", Slug: "platform-guild"},
		{Tag: "gchat-work", Hash8: "deadbeef"}:      {SpaceType: "dm", Slug: "jane-doe"},
		{Tag: "gchat-acme-corp", Hash8: "deadbeef"}: {SpaceType: "dm", Slug: "jane-doe"},
		{Tag: "gchat-work", Hash8: "12345678"}:      {SpaceType: "space", Slug: "a_deadbeef"},
	}
	if len(got) != len(want) {
		t.Fatalf("ScanChatStems = %v; want %v", got, want)
	}
	for key, info := range want {
		if got[key] != info {
			t.Errorf("ScanChatStems[%+v] = %+v; want %+v", key, got[key], info)
		}
	}

	// A missing root is an empty archive, not an error.
	if got, err := ScanChatStems(filepath.Join(root, "missing")); err != nil || len(got) != 0 {
		t.Errorf("ScanChatStems on missing root = %v, %v; want empty", got, err)
	}
}
