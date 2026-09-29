/**
 * UsageProvider for Hermes Agent.
 *
 * Hermes reports its session id through herdr's agent_session.kind=id, and
 * that id is the primary key of its own session store, so resolution is an
 * exact lookup with no cwd fallback: two panes in one directory are distinct
 * sessions. Hermes also records the backend it billed, so this adapter
 * implements the shared SessionBillingProvider contract and shared layers
 * read billing facts without knowing the harness.
 */
package hermes

import (
	"github.com/senna-lang/herdr-agent-usage/internal/core"
	"github.com/senna-lang/herdr-agent-usage/internal/provider"
)

type usageProvider struct{}

// Provider is the Hermes Agent UsageProvider.
var Provider usageProvider

func (usageProvider) AgentID() string { return "hermes" }

func (usageProvider) ResolveUsage(input provider.UsageResolveInput) *core.ContextUsage {
	sessionID := provider.SessionID(input)
	if sessionID == nil {
		return nil
	}
	return ResolveUsageIn(ResolveHome(), *sessionID)
}

// ResolveSessionBilling reports the session's own billing facts. Hermes
// persists them on the same row as its token counters, so one read answers
// both the context and the billing question.
func (p usageProvider) ResolveSessionBilling(input provider.UsageResolveInput) (string, string, int, float64, bool) {
	usage := p.ResolveUsage(input)
	if usage == nil {
		return "", "", 0, 0, false
	}
	return usage.BillingMode, usage.BillingProvider, usage.SessionTokens, usage.SessionCostUSD, true
}

var (
	_ provider.UsageProvider          = usageProvider{}
	_ provider.SessionBillingProvider = usageProvider{}
)
