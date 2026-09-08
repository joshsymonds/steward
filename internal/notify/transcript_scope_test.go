package notify

import (
	"reflect"
	"testing"
)

func TestTranscriptResultInventoryExcludesObsoleteOutputs(t *testing.T) {
	obsolete := map[string][]string{
		"ScanResult": {
			"LiveTasks", "Teammates", "LastSubstantiveUser", "FirstTimestamp",
			"LastTimestamp", "UserTurns", "BytesScanned",
		},
		"GoalState": {"Condition", "Iterations"},
	}
	types := map[string]reflect.Type{
		"ScanResult": reflect.TypeOf(ScanResult{}),
		"GoalState":  reflect.TypeOf(GoalState{}),
	}
	for typeName, fields := range obsolete {
		for _, field := range fields {
			if _, ok := types[typeName].FieldByName(field); ok {
				t.Errorf("%s still exposes obsolete field %s", typeName, field)
			}
		}
	}
}
