package cost

// rates holds USD-per-token pricing for one exact model id under one
// backend (anthropic list pricing or bedrock cross-region pricing).
type rates struct {
	input        float64
	output       float64
	cacheRead    float64
	cacheCreated float64
}

// cost returns the USD cost of one row's token usage under r. Unknown-model
// callers never reach here: lookup functions return ok=false instead of a
// zero rates value, so a genuinely-priced row is never silently charged $0
// for an unrecognized id.
func (r rates) cost(input, output, cacheRead, cacheCreated int) float64 {
	return float64(input)*r.input +
		float64(output)*r.output +
		float64(cacheRead)*r.cacheRead +
		float64(cacheCreated)*r.cacheCreated
}

// listRates returns the anthropic list (pay-as-you-go) USD-per-token rates
// for an exact model id, as billed when the caller is not on a Claude
// subscription. Model ids are matched exactly, never by family/prefix: a
// hypothetical "claude-opus-4-9" must not silently inherit "claude-opus-4-8"
// pricing just because it shares a prefix. ok is false for any id not in
// this table, which callers must treat as "$0, never guess".
func listRates(model string) (rates, bool) {
	switch model {
	case "claude-fable-5-1":
		// Same tier price as claude-fable-5, but cache reads are 0.025x input.
		return rates{0.00001, 0.00005, 0.00000025, 0.0000125}, true
	case "claude-fable-5":
		return rates{0.00001, 0.00005, 0.000001, 0.0000125}, true
	case "claude-opus-5-5":
		// Cache reads are 0.05x input.
		return rates{0.000004, 0.00002, 0.0000002, 0.000005}, true
	case "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-5":
		return rates{0.000005, 0.000025, 0.0000005, 0.00000625}, true
	case "claude-sonnet-5-5", "claude-sonnet-5":
		return rates{0.000002, 0.00001, 0.0000002, 0.0000025}, true
	case "claude-sonnet-4-6":
		return rates{0.000003, 0.000015, 0.0000003, 0.00000375}, true
	case "claude-haiku-4-5":
		return rates{0.000001, 0.000005, 0.0000001, 0.00000125}, true
	default:
		return rates{}, false
	}
}

// bedrockRates returns the bedrock (us. cross-region) USD-per-token rates
// for an exact model id, keyed by the same plain claude-* id the transcript
// reports on bedrock (the transcript never carries a bedrock-qualified
// model id). See listRates for the exact-match-only rationale.
func bedrockRates(model string) (rates, bool) {
	switch model {
	case "claude-fable-5-1":
		return rates{0.000011, 0.000055, 0.000000275, 0.00001375}, true
	case "claude-fable-5":
		return rates{0.000011, 0.000055, 0.0000011, 0.00001375}, true
	case "claude-opus-5-5":
		return rates{0.0000044, 0.000022, 0.00000022, 0.0000055}, true
	case "claude-opus-5", "claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-5":
		return rates{0.0000055, 0.0000275, 0.00000055, 0.000006875}, true
	case "claude-sonnet-5-5", "claude-sonnet-5":
		return rates{0.0000022, 0.000011, 0.00000022, 0.00000275}, true
	case "claude-sonnet-4-6":
		return rates{0.0000033, 0.0000165, 0.00000033, 0.000004125}, true
	case "claude-haiku-4-5":
		return rates{0.0000011, 0.0000055, 0.00000011, 0.000001375}, true
	default:
		return rates{}, false
	}
}
