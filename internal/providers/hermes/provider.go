/**
 * UsageProvider for Hermes Agent.
 *
 * Hermes reports its session id through herdr's agent_session.kind=id, and
 * that id is the primary key of its own session store, so resolution is an
 * exact lookup with no cwd fallback: two panes in one directory are distinct
 * sessions. Hermes also records the backend it billed, so this adapter
 * implements the shared SessionBillingProvider contract and translates
 * Hermes's own billing-route vocabulary into the shared classification.
 */
package hermes

import (
	"strings"

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
func (p usageProvider) ResolveSessionBilling(input provider.UsageResolveInput) (core.SessionBilling, bool) {
	usage := p.ResolveUsage(input)
	if usage == nil || usage.Billing == nil {
		return core.SessionBilling{}, false
	}
	return *usage.Billing, true
}

// Hermes billing routes that cover a session's spend under a plan the agent
// is already paying for (agent/usage_pricing.py resolve_billing_route).
var subscriptionBillingModes = map[string]bool{"subscription_included": true}

// classifyBillingMode maps one Hermes billing route to the shared class.
// Everything Hermes prices per token — its direct chat_completions route,
// official model APIs, and published-price snapshots — is pay-as-you-go.
// An unrecognised or absent route stays unknown so display fails open.
func classifyBillingMode(mode string) core.BillingClass {
	switch normalized := strings.ToLower(strings.TrimSpace(mode)); {
	case normalized == "" || normalized == "unknown":
		return core.BillingClassUnknown
	case subscriptionBillingModes[normalized]:
		return core.BillingClassSubscription
	default:
		return core.BillingClassPayAsYouGo
	}
}

var (
	_ provider.UsageProvider          = usageProvider{}
	_ provider.SessionBillingProvider = usageProvider{}
)
