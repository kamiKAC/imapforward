package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func openTestStore(t *testing.T) (*StateStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStateStore(path)
	if err != nil {
		t.Fatalf("OpenStateStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func mustFilter(t *testing.T, s *StateStore, id, folder string, uids ...imap.UID) []imap.UID {
	t.Helper()
	got, err := s.FilterUnforwarded(id, folder, uids)
	if err != nil {
		t.Fatalf("FilterUnforwarded: %v", err)
	}
	return got
}

func mustMark(t *testing.T, s *StateStore, id, folder string, uid imap.UID) {
	t.Helper()
	if err := s.MarkForwarded(id, folder, uid); err != nil {
		t.Fatalf("MarkForwarded: %v", err)
	}
}

func TestStateStore_MarkAndFilter(t *testing.T) {
	s, _ := openTestStore(t)
	mustMark(t, s, "a", "INBOX", 2)
	mustMark(t, s, "a", "INBOX", 4)

	got := mustFilter(t, s, "a", "INBOX", 1, 2, 3, 4, 5)
	if want := []imap.UID{1, 3, 5}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStateStore_FilterUnknownSourceReturnsAll(t *testing.T) {
	s, _ := openTestStore(t)
	got := mustFilter(t, s, "nobody", "INBOX", 7, 3, 5)
	if want := []imap.UID{7, 3, 5}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStateStore_PersistsAcrossReopen(t *testing.T) {
	s, path := openTestStore(t)
	mustMark(t, s, "a", "INBOX", 10)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	got := mustFilter(t, reopened, "a", "INBOX", 10, 11)
	if want := []imap.UID{11}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStateStore_IsolatedByTrackingIDAndFolder(t *testing.T) {
	s, _ := openTestStore(t)
	mustMark(t, s, "a", "INBOX", 1)

	if got := mustFilter(t, s, "b", "INBOX", 1); len(got) != 1 {
		t.Errorf("other trackingId should not see UID as forwarded, got %v", got)
	}
	if got := mustFilter(t, s, "a", "Other", 1); len(got) != 1 {
		t.Errorf("other folder should not see UID as forwarded, got %v", got)
	}
	if got := mustFilter(t, s, "a", "INBOX", 1); len(got) != 0 {
		t.Errorf("same trackingId and folder should see UID as forwarded, got %v", got)
	}
}

func TestStateStore_SyncUIDValidity(t *testing.T) {
	s, _ := openTestStore(t)

	reset, err := s.SyncUIDValidity("a", "INBOX", 100)
	if err != nil || reset {
		t.Fatalf("first sync: reset=%v err=%v, want false, nil", reset, err)
	}
	mustMark(t, s, "a", "INBOX", 1)
	mustMark(t, s, "a", "Other", 1)

	reset, err = s.SyncUIDValidity("a", "INBOX", 100)
	if err != nil || reset {
		t.Fatalf("same validity: reset=%v err=%v, want false, nil", reset, err)
	}
	if got := mustFilter(t, s, "a", "INBOX", 1); len(got) != 0 {
		t.Errorf("marks must survive an unchanged UIDVALIDITY, got %v", got)
	}

	reset, err = s.SyncUIDValidity("a", "INBOX", 200)
	if err != nil || !reset {
		t.Fatalf("changed validity: reset=%v err=%v, want true, nil", reset, err)
	}
	if got := mustFilter(t, s, "a", "INBOX", 1); len(got) != 1 {
		t.Errorf("marks must be cleared after UIDVALIDITY change, got %v", got)
	}
	if got := mustFilter(t, s, "a", "Other", 1); len(got) != 0 {
		t.Errorf("other folder must be untouched, got %v", got)
	}

	reset, err = s.SyncUIDValidity("a", "INBOX", 200)
	if err != nil || reset {
		t.Fatalf("after reset, same validity: reset=%v err=%v, want false, nil", reset, err)
	}
}

func TestStateStore_MarkIdempotent(t *testing.T) {
	s, _ := openTestStore(t)
	mustMark(t, s, "a", "INBOX", 5)
	mustMark(t, s, "a", "INBOX", 5)
	if got := mustFilter(t, s, "a", "INBOX", 5); len(got) != 0 {
		t.Errorf("got %v, want none", got)
	}
}

func TestStateStore_ConcurrentMark(t *testing.T) {
	s, _ := openTestStore(t)
	folders := []string{"INBOX", "Archive"}
	const perWorker = 100

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			folder := folders[w%len(folders)]
			for i := 0; i < perWorker; i++ {
				uid := imap.UID(w*perWorker + i + 1)
				if err := s.MarkForwarded("a", folder, uid); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("MarkForwarded: %v", err)
	}

	for fi, folder := range folders {
		var all []imap.UID
		for w := fi; w < 8; w += len(folders) {
			for i := 0; i < perWorker; i++ {
				all = append(all, imap.UID(w*perWorker+i+1))
			}
		}
		if got := mustFilter(t, s, "a", folder, all...); len(got) != 0 {
			t.Errorf("folder %s: %d UIDs not recorded", folder, len(got))
		}
	}
}

func TestOpenStateStore_BadPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "state.db")
	s, err := OpenStateStore(path)
	if err == nil {
		_ = s.Close()
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should mention the path, got: %v", err)
	}
}
