package notify

import (
	"container/list"
	"fmt"
	"testing"
	"time"
)

type fakeClaimClock struct{ now time.Time }

func (clock *fakeClaimClock) Now() time.Time { return clock.now }

func completionClaimKey(index int) claimKey {
	return claimKey{
		Harness: harnessPi, SessionID: "session", Kind: eventKindCompletion,
		CompletionID: fmt.Sprintf("completion-%05d", index),
	}
}

func TestClaimStoreAtomicallyDeduplicatesInflightAndSuccessfulClaims(t *testing.T) {
	clock := &fakeClaimClock{now: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	store := newClaimStore(clock.Now)
	key := completionClaimKey(1)

	token, admitted := store.claim(key)
	if !admitted || token == 0 {
		t.Fatal("first claim was not admitted")
	}
	if _, duplicate := store.claim(key); duplicate {
		t.Fatal("in-flight duplicate was admitted")
	}
	store.finish(key, token, true)
	if _, duplicate := store.claim(key); duplicate {
		t.Fatal("successful duplicate was admitted")
	}
	if got := len(store.entries); got != 1 {
		t.Fatalf("entries = %d, want 1", got)
	}
}

func TestClaimStoreFailedOwnerReleasesForLaterRetry(t *testing.T) {
	clock := &fakeClaimClock{now: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	store := newClaimStore(clock.Now)
	key := completionClaimKey(1)
	token, admitted := store.claim(key)
	if !admitted {
		t.Fatal("first claim rejected")
	}
	store.finish(key, token, false)
	if _, admitted = store.claim(key); !admitted {
		t.Fatal("failed claim was retained")
	}
}

func TestClaimStoreTTLIsSinceFirstClaimAndDuplicatesDoNotRefreshIt(t *testing.T) {
	clock := &fakeClaimClock{now: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	store := newClaimStore(clock.Now)
	key := completionClaimKey(1)
	if _, admitted := store.claim(key); !admitted {
		t.Fatal("first claim rejected")
	}
	clock.now = clock.now.Add(claimTTL - time.Nanosecond)
	if _, admitted := store.claim(key); admitted {
		t.Fatal("claim before 24h boundary was admitted")
	}
	clock.now = clock.now.Add(time.Nanosecond)
	if _, admitted := store.claim(key); !admitted {
		t.Fatal("claim at exact 24h boundary was not admitted")
	}
}

func TestClaimStoreEvictsOldestAtBoundAndBoundsOrderingStorage(t *testing.T) {
	clock := &fakeClaimClock{now: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	store := newClaimStore(clock.Now)
	oldest := completionClaimKey(0)
	for index := range maximumClaims {
		if _, admitted := store.claim(completionClaimKey(index)); !admitted {
			t.Fatalf("claim %d rejected", index)
		}
		clock.now = clock.now.Add(time.Nanosecond)
	}
	if _, admitted := store.claim(completionClaimKey(maximumClaims)); !admitted {
		t.Fatal("claim above capacity rejected instead of evicting")
	}
	if len(store.entries) != maximumClaims || store.order.Len() != maximumClaims {
		t.Fatalf("storage = map %d order %d, want both %d", len(store.entries), store.order.Len(), maximumClaims)
	}
	if _, exists := store.entries[oldest]; exists {
		t.Fatal("oldest claim was not evicted")
	}
	frontKey, valid := claimElementKey(store.order.Front())
	if !valid || frontKey != completionClaimKey(1) {
		t.Fatalf("oldest retained order entry = %#v", frontValue(store.order.Front()))
	}
}

func TestClaimStoreDuplicateDoesNotChangeOldestFirstOrder(t *testing.T) {
	clock := &fakeClaimClock{now: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	store := newClaimStore(clock.Now)
	first := completionClaimKey(1)
	second := completionClaimKey(2)
	_, _ = store.claim(first)
	clock.now = clock.now.Add(time.Second)
	_, _ = store.claim(second)
	clock.now = clock.now.Add(time.Second)
	if _, admitted := store.claim(first); admitted {
		t.Fatal("duplicate admitted")
	}
	got, valid := claimElementKey(store.order.Front())
	if !valid || got != first {
		t.Fatalf("duplicate refreshed order: front = %+v", got)
	}
}

func TestClaimStoreOldOwnerCannotReleaseReplacementAfterExpiry(t *testing.T) {
	clock := &fakeClaimClock{now: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	store := newClaimStore(clock.Now)
	key := completionClaimKey(1)
	oldToken, admitted := store.claim(key)
	if !admitted {
		t.Fatal("old claim rejected")
	}
	clock.now = clock.now.Add(claimTTL)
	newToken, admitted := store.claim(key)
	if !admitted || newToken == oldToken {
		t.Fatal("replacement claim not issued with unique ownership")
	}
	store.finish(key, oldToken, false)
	if _, replacementAdmitted := store.claim(key); replacementAdmitted {
		t.Fatal("old owner released replacement claim")
	}
}

func TestClaimStoreOldOwnerCannotReleaseReplacementAfterCapacityEviction(t *testing.T) {
	clock := &fakeClaimClock{now: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	store := newClaimStore(clock.Now)
	key := completionClaimKey(0)
	oldToken, admitted := store.claim(key)
	if !admitted {
		t.Fatal("old claim rejected")
	}
	for index := 1; index <= maximumClaims; index++ {
		if _, fillAdmitted := store.claim(completionClaimKey(index)); !fillAdmitted {
			t.Fatalf("fill claim %d rejected", index)
		}
	}
	newToken, admitted := store.claim(key)
	if !admitted || newToken == oldToken {
		t.Fatal("evicted key was not reclaimed with new ownership")
	}
	store.finish(key, oldToken, false)
	if _, replacementAdmitted := store.claim(key); replacementAdmitted {
		t.Fatal("evicted old owner released replacement claim")
	}
}

func TestClaimKeyScopesHarnessSessionKindAndCompletionID(t *testing.T) {
	clock := &fakeClaimClock{now: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	store := newClaimStore(clock.Now)
	base := completionClaimKey(1)
	variants := []claimKey{
		base,
		{Harness: harnessClaude, SessionID: base.SessionID, Kind: base.Kind, CompletionID: base.CompletionID},
		{Harness: base.Harness, SessionID: "other", Kind: base.Kind, CompletionID: base.CompletionID},
		{Harness: base.Harness, SessionID: base.SessionID, Kind: eventKindInput, CompletionID: base.CompletionID},
		{Harness: base.Harness, SessionID: base.SessionID, Kind: base.Kind, CompletionID: "other"},
	}
	for _, key := range variants {
		if _, admitted := store.claim(key); !admitted {
			t.Fatalf("distinct scoped key rejected: %+v", key)
		}
	}
}

func frontValue(element *list.Element) any {
	if element == nil {
		return nil
	}
	return element.Value
}
