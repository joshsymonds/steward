package notify

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func labelMetadataRecord(harness, session string) labelRecord {
	return labelRecord{
		Version:                          labelRecordVersion,
		Harness:                          harness,
		Session:                          session,
		Label:                            "Shared Session Label",
		SourceGeneration:                 9,
		LatestCompletionID:               "native-completion-9",
		ExchangeCount:                    9,
		LastSuccessfulRefreshExchange:    5,
		LastAttemptedMaterialFingerprint: strings.Repeat("a", 64),
	}
}

func writeLabelMetadataRecord(t *testing.T, stateBase string, record labelRecord, padding int) string {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if padding < len(data) {
		t.Fatalf("padding target %d is shorter than record %d", padding, len(data))
	}
	data = append(data, bytes.Repeat([]byte(" "), padding-len(data))...)
	directory := filepath.Join(stateBase, labelStateDirectoryName)
	if err = os.MkdirAll(directory, labelDirectoryMode); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(directory, labelDirectoryMode); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, labelSnapshotName(record.Harness, record.Session))
	if err = os.WriteFile(path, data, labelFileMode); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, labelFileMode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadLabelMetadataReturnsValidatedPublicProjection(t *testing.T) {
	tests := []struct {
		name   string
		record labelRecord
		want   LabelMetadata
	}{
		{
			name:   "full record keeps source and successful label generations distinct",
			record: labelMetadataRecord(harnessPi, "full-session"),
			want: LabelMetadata{
				Label: "Shared Session Label", CompletionID: "native-completion-9",
				SourceGeneration: 9, LabelGeneration: 5,
			},
		},
		{
			name: "maximum uint64 generations remain exact",
			record: func() labelRecord {
				record := labelMetadataRecord(harnessPi, "maximum-session")
				record.SourceGeneration = math.MaxUint64
				record.ExchangeCount = math.MaxUint64
				record.LastSuccessfulRefreshExchange = math.MaxUint64
				return record
			}(),
			want: LabelMetadata{
				Label: "Shared Session Label", CompletionID: "native-completion-9",
				SourceGeneration: math.MaxUint64, LabelGeneration: math.MaxUint64,
			},
		},
		{
			name: "successful KEEP advances the public label generation",
			record: func() labelRecord {
				record := labelMetadataRecord(harnessClaude, "keep-session")
				record.SourceGeneration = 13
				record.ExchangeCount = 13
				record.LastSuccessfulRefreshExchange = 13
				return record
			}(),
			want: LabelMetadata{
				Label: "Shared Session Label", CompletionID: "native-completion-9",
				SourceGeneration: 13, LabelGeneration: 13,
			},
		},
		{
			name: "known record may have an empty label and zero label generation",
			record: func() labelRecord {
				record := labelMetadataRecord(harnessPi, "empty-label-session")
				record.Label = ""
				record.SourceGeneration = 1
				record.ExchangeCount = 1
				record.LastSuccessfulRefreshExchange = 0
				return record
			}(),
			want: LabelMetadata{
				CompletionID: "native-completion-9", SourceGeneration: 1, LabelGeneration: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateBase := t.TempDir()
			writeLabelMetadataRecord(t, stateBase, tt.record, maximumLabelSnapshotBytes)
			got, present, err := ReadLabelMetadata(stateBase, tt.record.Harness, tt.record.Session)
			if err != nil || !present {
				t.Fatalf("ReadLabelMetadata present/error = %v/%v, want true/nil", present, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ReadLabelMetadata = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestReadLabelMetadataMissingStateNeverCreatesFiles(t *testing.T) {
	parent := t.TempDir()
	missingBase := filepath.Join(parent, "absent", "notify")
	metadata, present, err := ReadLabelMetadata(missingBase, harnessPi, "missing-session")
	if err != nil || present || metadata != (LabelMetadata{}) {
		t.Fatalf("absent base result = %+v/%v/%v", metadata, present, err)
	}
	if _, statErr := os.Lstat(filepath.Join(parent, "absent")); !os.IsNotExist(statErr) {
		t.Fatalf("read created absent state: %v", statErr)
	}

	existingBase := t.TempDir()
	before, err := os.ReadDir(existingBase)
	if err != nil {
		t.Fatal(err)
	}
	metadata, present, err = ReadLabelMetadata(existingBase, harnessPi, "missing-session")
	if err != nil || present || metadata != (LabelMetadata{}) {
		t.Fatalf("absent directory result = %+v/%v/%v", metadata, present, err)
	}
	after, err := os.ReadDir(existingBase)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("read changed existing state entries: before=%v after=%v", before, after)
	}
}

func TestReadLabelMetadataRejectsCorruptUnsafeAndOversizedWithoutMutation(t *testing.T) {
	const session = "rejected-session"
	tests := []struct {
		name  string
		setup func(*testing.T, string, labelRecord) string
	}{
		{
			name: "corrupt strict JSON",
			setup: func(t *testing.T, stateBase string, record labelRecord) string {
				t.Helper()
				path := writeLabelMetadataRecord(t, stateBase, record, 512)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				wire := strings.Replace(string(data), `"version":1`, `"version":1,"unknown":true`, 1)
				if err = os.WriteFile(path, []byte(wire), labelFileMode); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "unsafe file mode",
			setup: func(t *testing.T, stateBase string, record labelRecord) string {
				t.Helper()
				path := writeLabelMetadataRecord(t, stateBase, record, 512)
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "unsafe label directory mode",
			setup: func(t *testing.T, stateBase string, record labelRecord) string {
				t.Helper()
				path := writeLabelMetadataRecord(t, stateBase, record, 512)
				if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "4097-byte valid JSON",
			setup: func(t *testing.T, stateBase string, record labelRecord) string {
				t.Helper()
				return writeLabelMetadataRecord(t, stateBase, record, maximumLabelSnapshotBytes+1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateBase := t.TempDir()
			record := labelMetadataRecord(harnessPi, session)
			path := tt.setup(t, stateBase, record)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			metadata, present, readErr := ReadLabelMetadata(stateBase, record.Harness, record.Session)
			if readErr == nil || present || metadata != (LabelMetadata{}) {
				t.Fatalf("rejected result = %+v/%v/%v, want empty/false/error", metadata, present, readErr)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			afterInfo, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) || afterInfo.ModTime() != beforeInfo.ModTime() ||
				afterInfo.Mode() != beforeInfo.Mode() {
				t.Fatalf("rejected snapshot changed: before=%q/%v after=%q/%v", before, beforeInfo, after, afterInfo)
			}
		})
	}

	t.Run("snapshot symlink", func(t *testing.T) {
		stateBase := t.TempDir()
		record := labelMetadataRecord(harnessPi, session)
		outside := filepath.Join(t.TempDir(), "outside.json")
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(outside, data, labelFileMode); err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(stateBase, labelStateDirectoryName)
		if err = os.Mkdir(directory, labelDirectoryMode); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, labelSnapshotName(record.Harness, record.Session))
		if err = os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		if metadata, present, readErr := ReadLabelMetadata(stateBase, record.Harness, record.Session); readErr == nil ||
			present || metadata != (LabelMetadata{}) {
			t.Fatalf("symlink result = %+v/%v/%v", metadata, present, readErr)
		}
		if got, readErr := os.ReadFile(outside); readErr != nil || !bytes.Equal(got, data) {
			t.Fatalf("outside target changed: %v %q", readErr, got)
		}
	})
}

func TestReadLabelMetadataScopesExactHarnessSessionPair(t *testing.T) {
	stateBase := t.TempDir()
	record := labelMetadataRecord(harnessPi, "shared-native-id")
	writeLabelMetadataRecord(t, stateBase, record, 512)

	for _, scope := range []struct{ harness, session string }{
		{harnessClaude, record.Session},
		{record.Harness, "different-native-id"},
	} {
		metadata, present, err := ReadLabelMetadata(stateBase, scope.harness, scope.session)
		if err != nil || present || metadata != (LabelMetadata{}) {
			t.Errorf(
				"cross-scope %s/%s = %+v/%v/%v, want missing",
				scope.harness, scope.session, metadata, present, err,
			)
		}
	}
}

func TestReadLabelMetadataRepeatedReadsLeaveContentMetadataAndCountersUnchanged(t *testing.T) {
	stateBase := t.TempDir()
	record := labelMetadataRecord(harnessPi, "repeat-session")
	path := writeLabelMetadataRecord(t, stateBase, record, 768)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	for range 5 {
		metadata, present, readErr := ReadLabelMetadata(stateBase, record.Harness, record.Session)
		if readErr != nil || !present || metadata.SourceGeneration != record.SourceGeneration ||
			metadata.LabelGeneration != record.LastSuccessfulRefreshExchange {
			t.Fatalf("repeated result = %+v/%v/%v", metadata, present, readErr)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || afterInfo.ModTime() != beforeInfo.ModTime() ||
		afterInfo.Mode() != beforeInfo.Mode() || afterInfo.Size() != beforeInfo.Size() {
		t.Fatalf("repeated reads changed snapshot: before=%v after=%v", beforeInfo, afterInfo)
	}
	decoded, valid := decodeLabelRecord(after, record.Harness, record.Session)
	if !valid || decoded.SourceGeneration != 9 || decoded.ExchangeCount != 9 ||
		decoded.LastSuccessfulRefreshExchange != 5 {
		t.Fatalf("counters changed after reads: %+v valid=%v", decoded, valid)
	}
}

func TestReadLabelMetadataValidatesScopeBeforeOpeningState(t *testing.T) {
	parent := t.TempDir()
	stateBase := filepath.Join(parent, "must-not-be-opened")
	for _, scope := range []struct{ harness, session string }{
		{"other", "session"},
		{harnessPi, ""},
		{harnessPi, strings.Repeat("x", maximumPreparedIDBytes+1)},
		{harnessPi, "control\nsession"},
		{harnessPi, string([]byte{0xff})},
	} {
		metadata, present, err := ReadLabelMetadata(stateBase, scope.harness, scope.session)
		if err == nil || present || metadata != (LabelMetadata{}) {
			t.Errorf("invalid scope %q/%q = %+v/%v/%v", scope.harness, scope.session, metadata, present, err)
		}
	}
	if _, err := os.Lstat(stateBase); !os.IsNotExist(err) {
		t.Fatalf("invalid reads created state base: %v", err)
	}
}
