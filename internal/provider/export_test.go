package provider

import "context"

// AdmitForTest exposes admit for the external test package.
func (r *Router) AdmitForTest(c Capability) (Ticket, error) {
	tk, rel, err := r.admit(context.Background(), c)
	if err != nil {
		return Ticket{}, err
	}
	rel()
	return tk, nil
}

// BreakerForTest returns the breaker of a (vendor, capability).
func (r *Router) BreakerForTest(p Name, c Capability) *Breaker { return r.breakers[key{p, c}] }

// LLMModelForTest, TTSVendorsForTest and TextModelForTest expose the selection helpers.
func (r *Router) LLMModelForTest() string                 { return r.llmModel() }
func (r *Router) TTSVendorsForTest() []Name               { return r.ttsVendors() }
func (r *Router) TextModelForTest(req TextRequest) string { return r.textModel(req) }

// RotationBufferBytesForTest exposes the rotation buffer size.
const RotationBufferBytesForTest = rotationBufferBytes
