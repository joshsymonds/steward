package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func labelTestEvent(harness, session, completion, user, assistant string) PreparedEvent {
	sourceEvent := eventTurnComplete
	if harness == harnessClaude {
		sourceEvent = eventStop
	}
	return PreparedEvent{
		Version: preparedEventVersion, Harness: harness, SessionID: session,
		Kind: eventKindCompletion, SourceEvent: sourceEvent,
		CompletionID: completion, CWD: "/work/SECRET-project",
		User: user, Assistant: assistant,
	}
}

func onlyLabelSnapshot(t *testing.T, stateBase string) (string, []byte) {
	t.Helper()
	directory := filepath.Join(stateBase, labelStateDirectoryName)
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("reading label directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("label directory entries = %d, want one: %+v", len(entries), entries)
	}
	path := filepath.Join(directory, entries[0].Name())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading label snapshot: %v", err)
	}
	return path, data
}

func decodeLabelSnapshotForTest(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var snapshot map[string]any
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatalf("decoding label snapshot: %v", err)
	}
	return snapshot
}

func TestLabelStorePersistsMinimalOwnerOnlySnapshotAndReconstructs(t *testing.T) {
	stateBase := t.TempDir()
	store := NewLabelStore(stateBase)
	event := labelTestEvent(
		harnessPi, "../../session/with separators", "native-completion-1",
		"SECRET user source", "SECRET assistant source",
	)
	plan, err := store.planCompletion(event)
	if err != nil {
		t.Fatal(err)
	}
	if plan.current != "" || !plan.refresh || plan.sourceGeneration != 1 || plan.exchangeCount != 1 {
		t.Fatalf("initial plan = %+v", plan)
	}
	if err = store.finishCompletion(plan, "Shared Session Label"); err != nil {
		t.Fatal(err)
	}

	path, data := onlyLabelSnapshot(t, stateBase)
	requireMinimalLabelSnapshot(t, path, data, event)
	requireOwnerOnlyLabelPaths(t, stateBase, path)

	reconstructed := NewLabelStore(stateBase)
	label, err := reconstructed.lookupLabel(event.Harness, event.SessionID)
	if err != nil || label != "Shared Session Label" {
		t.Fatalf("reconstructed label/error = %q/%v", label, err)
	}
	next, err := reconstructed.planCompletion(labelTestEvent(
		event.Harness, event.SessionID, "native-completion-2", "new user", "new assistant",
	))
	if err != nil {
		t.Fatal(err)
	}
	if next.current != "Shared Session Label" || next.refresh || next.sourceGeneration != 2 || next.exchangeCount != 2 {
		t.Fatalf("reconstructed next plan = %+v", next)
	}
}

func requireMinimalLabelSnapshot(t *testing.T, path string, data []byte, event PreparedEvent) {
	t.Helper()
	if len(data) > maximumLabelSnapshotBytes {
		t.Fatalf("snapshot size = %d, want <= %d", len(data), maximumLabelSnapshotBytes)
	}
	base := filepath.Base(path)
	if len(base) != 64+len(labelSnapshotSuffix) || !strings.HasSuffix(base, labelSnapshotSuffix) ||
		strings.Contains(base, "session") || strings.Contains(base, "/") {
		t.Fatalf("snapshot filename = %q, want SHA256 only", base)
	}
	for _, secret := range []string{
		"SECRET user source", "SECRET assistant source", "SECRET-project", "notification", "body",
	} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("snapshot leaked %q: %s", secret, data)
		}
	}
	wantFields := []string{
		"version", "harness", "session", "label", "source_generation",
		"latest_completion_id", "exchange_count", "last_successful_refresh_exchange",
		"last_attempted_material_fingerprint",
	}
	snapshot := decodeLabelSnapshotForTest(t, data)
	for _, name := range wantFields {
		if _, ok := snapshot[name]; !ok {
			t.Errorf("snapshot missing %q: %s", name, data)
		}
	}
	if len(snapshot) != len(wantFields) {
		t.Fatalf("snapshot field count = %d, want exactly %d", len(snapshot), len(wantFields))
	}
	if snapshot["harness"] != harnessPi || snapshot["session"] != event.SessionID ||
		snapshot["label"] != "Shared Session Label" ||
		snapshot["latest_completion_id"] != event.CompletionID ||
		snapshot["source_generation"] != float64(1) || snapshot["exchange_count"] != float64(1) ||
		snapshot["last_successful_refresh_exchange"] != float64(1) {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	fingerprint, ok := snapshot["last_attempted_material_fingerprint"].(string)
	if !ok || len(fingerprint) != 64 || strings.Contains(fingerprint, "SECRET") {
		t.Fatalf("material fingerprint = %#v", snapshot["last_attempted_material_fingerprint"])
	}
}

func requireOwnerOnlyLabelPaths(t *testing.T, stateBase, snapshotPath string) {
	t.Helper()
	paths := map[string]os.FileMode{
		filepath.Join(stateBase, labelStateDirectoryName): labelDirectoryMode,
		snapshotPath: labelFileMode,
	}
	for target, wantMode := range paths {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if info.Mode().Perm() != wantMode || !ok || int(stat.Uid) != os.Getuid() {
			t.Fatalf(
				"%s mode/owner = %o/%v, want %o/uid%d",
				target, info.Mode().Perm(), ok, wantMode, os.Getuid(),
			)
		}
	}
}

func validLabelSnapshotWire(harness, session string) string {
	return fmt.Sprintf(
		`{"version":1,"harness":%q,"session":%q,"label":"Shared Session Label",`+
			`"source_generation":1,"latest_completion_id":"completion-1","exchange_count":1,`+
			`"last_successful_refresh_exchange":1,`+
			`"last_attempted_material_fingerprint":"%s"}`,
		harness, session, strings.Repeat("a", 64),
	)
}

func writeRawLabelSnapshot(t *testing.T, stateBase, session, wire string, mode os.FileMode) string {
	t.Helper()
	directory := filepath.Join(stateBase, labelStateDirectoryName)
	if err := os.MkdirAll(directory, labelDirectoryMode); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, labelSnapshotName(harnessPi, session))
	if err := os.WriteFile(path, []byte(wire), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLabelStoreRejectsStrictCorruptAndUnsafeSnapshots(t *testing.T) {
	const session = "strict-session"
	valid := validLabelSnapshotWire(harnessPi, session)
	tests := []struct {
		name   string
		mutate func(string) string
	}{
		{
			name:   "unknown field",
			mutate: func(wire string) string { return strings.Replace(wire, `}`, `,"extra":true}`, 1) },
		},
		{
			name:   "duplicate field",
			mutate: func(wire string) string { return strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1) },
		},
		{
			name:   "wrong version",
			mutate: func(wire string) string { return strings.Replace(wire, `"version":1`, `"version":2`, 1) },
		},
		{name: "wrong counter type", mutate: func(wire string) string {
			return strings.Replace(wire, `"exchange_count":1`, `"exchange_count":"1"`, 1)
		}},
		{name: "null counter", mutate: func(wire string) string {
			return strings.Replace(wire, `"source_generation":1`, `"source_generation":null`, 1)
		}},
		{name: "null scalar", mutate: func(wire string) string {
			return strings.Replace(wire, `"label":"Shared Session Label"`, `"label":null`, 1)
		}},
		{
			name:   "invalid harness",
			mutate: func(wire string) string { return strings.Replace(wire, `"harness":"pi"`, `"harness":"other"`, 1) },
		},
		{name: "mismatched scope", mutate: func(wire string) string {
			return strings.Replace(wire, `"session":"strict-session"`, `"session":"other-session"`, 1)
		}},
		{
			name:   "invalid label",
			mutate: func(wire string) string { return strings.Replace(wire, `Shared Session Label`, `two words`, 1) },
		},
		{
			name: "unpaired surrogate",
			mutate: func(wire string) string {
				return strings.Replace(wire, `Shared Session Label`, `Shared Session \ud800`, 1)
			},
		},
		{
			name:   "invalid completion",
			mutate: func(wire string) string { return strings.Replace(wire, `completion-1`, ``, 1) },
		},
		{name: "zero generation", mutate: func(wire string) string {
			return strings.Replace(wire, `"source_generation":1`, `"source_generation":0`, 1)
		}},
		{
			name:   "counter mismatch",
			mutate: func(wire string) string { return strings.Replace(wire, `"exchange_count":1`, `"exchange_count":2`, 1) },
		},
		{name: "refresh beyond count", mutate: func(wire string) string {
			return strings.Replace(
				wire,
				`"last_successful_refresh_exchange":1`,
				`"last_successful_refresh_exchange":2`,
				1,
			)
		}},
		{
			name:   "invalid fingerprint",
			mutate: func(wire string) string { return strings.Replace(wire, strings.Repeat("a", 64), `not-a-sha256`, 1) },
		},
		{name: "trailing JSON", mutate: func(wire string) string { return wire + `{}` }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateBase := t.TempDir()
			path := writeRawLabelSnapshot(t, stateBase, session, tt.mutate(valid), labelFileMode)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			store := NewLabelStore(stateBase)
			if label, lookupErr := store.lookupLabel(harnessPi, session); lookupErr == nil || label != "" {
				t.Fatalf("lookup label/error = %q/%v, want unavailable", label, lookupErr)
			}
			if _, planErr := store.planCompletion(labelTestEvent(
				harnessPi, session, "completion-2", "user", "assistant",
			)); planErr == nil {
				t.Fatal("corrupt snapshot accepted for update")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("corrupt snapshot overwritten: before=%s after=%s", before, after)
			}
		})
	}

	t.Run("wrong file mode", func(t *testing.T) {
		stateBase := t.TempDir()
		writeRawLabelSnapshot(t, stateBase, session, valid, 0o644)
		if _, err := NewLabelStore(stateBase).lookupLabel(harnessPi, session); err == nil {
			t.Fatal("world-readable snapshot accepted")
		}
	})

	t.Run("non-regular file", func(t *testing.T) {
		stateBase := t.TempDir()
		directory := filepath.Join(stateBase, labelStateDirectoryName)
		if err := os.MkdirAll(
			filepath.Join(directory, labelSnapshotName(harnessPi, session)),
			labelDirectoryMode,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := NewLabelStore(stateBase).lookupLabel(harnessPi, session); err == nil {
			t.Fatal("directory snapshot accepted")
		}
	})

	t.Run("snapshot symlink", func(t *testing.T) {
		stateBase := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside.json")
		if err := os.WriteFile(outside, []byte(valid), labelFileMode); err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(stateBase, labelStateDirectoryName)
		if err := os.MkdirAll(directory, labelDirectoryMode); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, labelSnapshotName(harnessPi, session))
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		store := NewLabelStore(stateBase)
		if _, err := store.lookupLabel(harnessPi, session); err == nil {
			t.Fatal("symlink snapshot accepted")
		}
		if _, err := store.planCompletion(labelTestEvent(harnessPi, session, "completion-2", "u", "a")); err == nil {
			t.Fatal("symlink snapshot accepted for update")
		}
		outsideData, err := os.ReadFile(outside)
		if err != nil || string(outsideData) != valid {
			t.Fatalf("outside target changed: %v %s", err, outsideData)
		}
	})

	t.Run("state directory symlink", func(t *testing.T) {
		stateBase := t.TempDir()
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(stateBase, labelStateDirectoryName)); err != nil {
			t.Fatal(err)
		}
		if _, err := NewLabelStore(stateBase).planCompletion(labelTestEvent(
			harnessPi, session, "completion-1", "u", "a",
		)); err == nil {
			t.Fatal("symlink state directory accepted")
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("outside directory mutated: %v %+v", err, entries)
		}
	})

	t.Run("unsafe state directory mode", func(t *testing.T) {
		stateBase := t.TempDir()
		directory := filepath.Join(stateBase, labelStateDirectoryName)
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := NewLabelStore(stateBase).planCompletion(labelTestEvent(
			harnessPi, session, "completion-1", "u", "a",
		)); err == nil {
			t.Fatal("unsafe state directory accepted")
		}
	})
}

func TestLabelStoreSnapshotSizeBoundary(t *testing.T) {
	const session = "size-boundary"
	valid := validLabelSnapshotWire(harnessPi, session)
	if len(valid) >= maximumLabelSnapshotBytes {
		t.Fatalf("valid fixture is unexpectedly large: %d bytes", len(valid))
	}

	t.Run("accepts 4096 valid JSON bytes", func(t *testing.T) {
		stateBase := t.TempDir()
		wire := valid + strings.Repeat(" ", maximumLabelSnapshotBytes-len(valid))
		path := writeRawLabelSnapshot(t, stateBase, session, wire, labelFileMode)
		store := NewLabelStore(stateBase)
		if label, err := store.lookupLabel(harnessPi, session); err != nil || label != "Shared Session Label" {
			t.Fatalf("boundary lookup label/error = %q/%v", label, err)
		}
		plan, err := store.planCompletion(labelTestEvent(
			harnessPi, session, "completion-1", "same", "source",
		))
		if err != nil || plan.current != "Shared Session Label" || plan.refresh {
			t.Fatalf("boundary plan/error = %+v/%v", plan, err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != wire {
			t.Fatal("accepted boundary snapshot was rewritten")
		}
	})

	t.Run("rejects 4097 valid JSON bytes without overwrite", func(t *testing.T) {
		stateBase := t.TempDir()
		wire := valid + strings.Repeat(" ", maximumLabelSnapshotBytes+1-len(valid))
		path := writeRawLabelSnapshot(t, stateBase, session, wire, labelFileMode)
		store := NewLabelStore(stateBase)
		if _, err := store.lookupLabel(harnessPi, session); err == nil {
			t.Fatal("oversized valid JSON snapshot accepted by lookup")
		}
		if _, err := store.planCompletion(labelTestEvent(
			harnessPi, session, "completion-2", "user", "assistant",
		)); err == nil {
			t.Fatal("oversized valid JSON snapshot accepted for update")
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != wire {
			t.Fatal("oversized valid JSON snapshot was overwritten")
		}
	})
}

func TestLabelStoreCounterOverflowIsUnavailableWithoutMutation(t *testing.T) {
	const session = "counter-boundary"
	counterWire := func(counter uint64) string {
		wire := validLabelSnapshotWire(harnessPi, session)
		value := fmt.Sprintf("%d", counter)
		wire = strings.Replace(wire, `"source_generation":1`, `"source_generation":`+value, 1)
		return strings.Replace(wire, `"exchange_count":1`, `"exchange_count":`+value, 1)
	}

	t.Run("maximum", func(t *testing.T) {
		stateBase := t.TempDir()
		path := writeRawLabelSnapshot(t, stateBase, session, counterWire(math.MaxUint64), labelFileMode)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		store := NewLabelStore(stateBase)
		if label, lookupErr := store.lookupLabel(harnessPi, session); lookupErr != nil ||
			label != "Shared Session Label" {
			t.Fatalf("maximum snapshot lookup = %q/%v", label, lookupErr)
		}
		if _, planErr := store.planCompletion(labelTestEvent(
			harnessPi, session, "completion-2", "user-2", "assistant-2",
		)); planErr == nil || planErr.Error() != labelUnavailableError().Error() {
			t.Fatalf("maximum counter plan error = %v, want fixed unavailable", planErr)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, before) {
			t.Fatalf("maximum counter snapshot mutated: before=%s after=%s", before, after)
		}
	})

	t.Run("maximum minus one advances once", func(t *testing.T) {
		stateBase := t.TempDir()
		path := writeRawLabelSnapshot(t, stateBase, session, counterWire(math.MaxUint64-1), labelFileMode)
		store := NewLabelStore(stateBase)
		plan, err := store.planCompletion(labelTestEvent(
			harnessPi, session, "completion-2", "user-2", "assistant-2",
		))
		if err != nil {
			t.Fatal(err)
		}
		if plan.sourceGeneration != math.MaxUint64 || plan.exchangeCount != math.MaxUint64 {
			t.Fatalf("boundary plan = %+v, want counters at maximum", plan)
		}
		atMaximum, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		record, valid := decodeLabelRecord(atMaximum, harnessPi, session)
		if !valid || record.SourceGeneration != math.MaxUint64 ||
			record.ExchangeCount != math.MaxUint64 || record.LatestCompletionID != "completion-2" {
			t.Fatalf("advanced maximum snapshot = %+v valid=%v", record, valid)
		}
		if _, planErr := store.planCompletion(labelTestEvent(
			harnessPi, session, "completion-3", "user-3", "assistant-3",
		)); planErr == nil || planErr.Error() != labelUnavailableError().Error() {
			t.Fatalf("post-maximum plan error = %v, want fixed unavailable", planErr)
		}
		afterRejectedPlan, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(afterRejectedPlan, atMaximum) {
			t.Fatalf("post-maximum rejection mutated snapshot: before=%s after=%s", atMaximum, afterRejectedPlan)
		}
	})
}

func TestLabelStoreAtomicSnapshotsNeverExposeTornJSON(t *testing.T) {
	stateBase := t.TempDir()
	writer := NewLabelStore(stateBase)
	reader := NewLabelStore(stateBase)
	first := labelTestEvent(harnessPi, "atomic-session", "completion-1", "user-1", "assistant-1")
	plan, err := writer.planCompletion(first)
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.finishCompletion(plan, "Atomic Session Label"); err != nil {
		t.Fatal(err)
	}

	const updates = 100
	errors := make(chan error, updates*2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		for index := 2; index <= updates; index++ {
			event := labelTestEvent(
				harnessPi, first.SessionID, fmt.Sprintf("completion-%d", index),
				fmt.Sprintf("user-%d", index), fmt.Sprintf("assistant-%d", index),
			)
			planned, planErr := writer.planCompletion(event)
			if planErr != nil {
				errors <- planErr
				return
			}
			if planned.refresh {
				if finishErr := writer.finishCompletion(planned, "Atomic Session Label"); finishErr != nil {
					errors <- finishErr
					return
				}
			}
		}
	}()
	go func() {
		defer wait.Done()
		for range updates * 3 {
			label, lookupErr := reader.lookupLabel(harnessPi, first.SessionID)
			if lookupErr != nil {
				errors <- lookupErr
				return
			}
			if label != "Atomic Session Label" {
				errors <- fmt.Errorf("label = %q", label)
				return
			}
		}
	}()
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}

	_, data := onlyLabelSnapshot(t, stateBase)
	if !json.Valid(data) {
		t.Fatalf("final snapshot is torn: %q", data)
	}
	entries, err := os.ReadDir(filepath.Join(stateBase, labelStateDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temporary snapshot left behind: %s", entry.Name())
		}
	}
}

func TestLabelStoreScopesExactHarnessSessionPair(t *testing.T) {
	store := NewLabelStore(t.TempDir())
	for _, scope := range []struct{ harness, session, label string }{
		{harnessPi, "same", "Pi Shared Session"},
		{harnessClaude, "same", "Claude Shared Session"},
		{harnessPi, "other", "Other Pi Session"},
	} {
		plan, err := store.planCompletion(labelTestEvent(scope.harness, scope.session, "id", "u", scope.label))
		if err != nil {
			t.Fatal(err)
		}
		if err = store.finishCompletion(plan, scope.label); err != nil {
			t.Fatal(err)
		}
	}
	for _, scope := range []struct{ harness, session, label string }{
		{harnessPi, "same", "Pi Shared Session"},
		{harnessClaude, "same", "Claude Shared Session"},
		{harnessPi, "other", "Other Pi Session"},
	} {
		label, err := store.lookupLabel(scope.harness, scope.session)
		if err != nil || label != scope.label {
			t.Errorf("lookup %s/%s = %q/%v, want %q", scope.harness, scope.session, label, err, scope.label)
		}
	}
}
