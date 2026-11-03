package provider

import "testing"

func TestEffortFromVariant(t *testing.T) {
	cases := map[string]string{
		"":          "medium", // unset = previous hardcoded default
		"medium":    "medium",
		"default":   "medium",
		"normal":    "medium",
		"2":         "medium",
		"minimal":   "minimal",
		"MIN":       "minimal",
		"none":      "minimal",
		"off":       "minimal",
		"0":         "minimal",
		"low":       "low",
		"1":         "low",
		"high":      "high",
		"HIGH":      "high",
		"max":       "high",
		"ultra":     "high",
		"3":         "high",
		"gibberish": "medium", // unknown variants are medium, not an error
	}
	for in, want := range cases {
		if got := EffortFromVariant(in); got != want {
			t.Errorf("EffortFromVariant(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenAIEffort(t *testing.T) {
	for in, want := range map[string]string{
		"minimal": "minimal", "low": "low", "medium": "medium", "high": "high",
		"": "medium", "bogus": "medium",
	} {
		if got := openaiEffort(in); got != want {
			t.Errorf("openaiEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAnthropicBudget(t *testing.T) {
	// maxTokens 8192: minimal is the 1024 floor, low 25%, medium 50%, high 80%
	for in, want := range map[string]int{
		"minimal": 1024, "low": 2048, "medium": 4096, "high": 6553, "": 4096,
	} {
		if got := anthropicBudget(in, 8192); got != want {
			t.Errorf("anthropicBudget(%q, 8192) = %d, want %d", in, got, want)
		}
	}
	// the budget must stay below max_tokens (the builder feeds this function
	// maxInt(req.MaxTokens, 8192), so the degenerate tiny-max case only shows
	// that the upper bound wins over the floor)
	if got := anthropicBudget("high", 1024); got >= 1024 {
		t.Errorf("anthropicBudget(high, 1024) = %d, want < 1024", got)
	}
}

func TestGeminiBudget(t *testing.T) {
	for in, want := range map[string]int{
		"minimal": 512, "low": 2048, "medium": 8192, "high": 24576, "": 8192,
	} {
		if got := geminiBudget(in); got != want {
			t.Errorf("geminiBudget(%q) = %d, want %d", in, got, want)
		}
	}
}
