package catalog

import "testing"

func TestParseVersion(t *testing.T) {
	older, err := ParseVersion("2026-10-05T14:22:45Z")
	if err != nil {
		t.Fatal(err)
	}
	newer, err := ParseVersion("2026-10-06T00:00:00Z")
	if err != nil || !newer.After(older) {
		t.Fatalf("newer = %v (%v), want after %v", newer, err, older)
	}
	for _, v := range []string{"", "v10", "2026-10-05", "2026-10-05T14:22:45+02:00"} {
		if _, err := ParseVersion(v); err == nil {
			t.Errorf("ParseVersion(%q) accepted a version outside the layout", v)
		}
	}
}

func TestSnapshotVersionParses(t *testing.T) {
	snap, err := SnapshotV4()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseVersion(snap.Version); err != nil {
		t.Fatal(err)
	}
}
