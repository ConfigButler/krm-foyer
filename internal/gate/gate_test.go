package gate

import (
	"net/http"
	"testing"

	"github.com/ConfigButler/krm-foyer/internal/interruption"
)

type anyone struct{}

func (anyone) Token(*http.Request) (Credential, *interruption.Interruption) {
	return Credential{Token: "t"}, nil
}

// A gate needs a credential source, and bounds that are positive or left at their
// defaults.
func TestNewRejectsBadConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no credential source":          {},
		"negative check interval":       {Credentials: anyone{}, SessionCheckInterval: -1},
		"negative response duration":    {Credentials: anyone{}, MaxResponseDuration: -1},
		"negative per-session requests": {Credentials: anyone{}, MaxSessionConcurrentRequests: -1},
		"negative replica requests":     {Credentials: anyone{}, MaxConcurrentRequests: -1},
		"negative rate":                 {Credentials: anyone{}, SessionRequestRate: -1},
		"negative burst":                {Credentials: anyone{}, SessionRequestBurst: -1},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	if _, err := New(Config{Credentials: anyone{}}); err != nil {
		t.Errorf("defaults: %v", err)
	}
}
