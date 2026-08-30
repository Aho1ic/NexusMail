//go:build sqlite_fts5

package imap

import (
	"slices"
	"testing"
)

// TestStaleUIDs covers the expunge decision on its own, including the case that
// used to panic: a provider answering with UIDs that were never asked about made
// the old len(stored)-len(present) capacity negative, and "makeslice: cap out of
// range" killed the body worker that was driving the pass.
func TestStaleUIDs(t *testing.T) {
	cases := []struct {
		name    string
		stored  []uint32
		present []uint32
		want    []uint32
	}{
		{
			name:    "provider echoes more uids than were requested",
			stored:  []uint32{1, 2},
			present: []uint32{1, 2, 3, 4},
			want:    []uint32{},
		},
		{
			name:    "unrequested uids do not mask a real expunge",
			stored:  []uint32{1, 2, 3},
			present: []uint32{1, 3, 90, 91},
			want:    []uint32{2},
		},
		{
			name:    "missing uids are stale",
			stored:  []uint32{4, 5, 6},
			present: []uint32{5},
			want:    []uint32{4, 6},
		},
		{
			name:    "nothing came back so everything is stale",
			stored:  []uint32{7, 8},
			present: nil,
			want:    []uint32{7, 8},
		},
		{
			name:    "duplicate uids in the response are not counted twice",
			stored:  []uint32{1, 2, 3},
			present: []uint32{1, 1, 1, 2},
			want:    []uint32{3},
		},
		{
			name:    "everything still present",
			stored:  []uint32{1, 2, 3},
			present: []uint32{3, 2, 1},
			want:    []uint32{},
		},
		{
			name:    "empty snapshot",
			stored:  nil,
			present: []uint32{1, 2},
			want:    []uint32{},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := staleUIDs(test.stored, test.present)
			if !slices.Equal(got, test.want) {
				t.Fatalf("staleUIDs(%v, %v) = %v, want %v", test.stored, test.present, got, test.want)
			}
		})
	}
}
