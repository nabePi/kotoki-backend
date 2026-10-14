package provider

// PriceTable estimates USD cost (spec 8.7.2). It is a port on the consumer side:
// *pricing.Table implements it and cmd/* injects it via RouterDeps.Prices. A combination
// without a price returns (0, false); estimation never fails.
type PriceTable interface {
	LLM(p Name, c Capability, model string, u TokenUsage) (usd float64, ok bool)
	TTS(p Name, model string, characters int) (float64, bool)
	Audio(p Name, c Capability, model string, audioMs int64) (float64, bool)
}

// ModelRef names one configured (vendor, capability, model) combination.
type ModelRef struct {
	Provider   Name
	Capability Capability
	Model      string
}
