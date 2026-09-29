/**
 * Reads one Hermes Agent session from a profile-local state database.
 *
 * Hermes stores every session in SQLite under its profile home, so the
 * adapter opens that database read-only and resolves exactly the session id
 * Herdr reported. Column presence is probed at runtime because the Hermes
 * schema gains columns across releases; a missing optional column degrades
 * one field rather than the whole read.
 */
package hermes

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/senna-lang/herdr-agent-usage/internal/core"
	_ "modernc.org/sqlite"
)

type sessionRow struct {
	model, modelConfig, billingProvider, billingBaseURL, billingMode string
	input, output, cacheRead, cacheWrite                             int
	actualCost, estimatedCost                                        float64
}

type message struct {
	Role, Content, APIContent, ToolCallID string
	ToolCalls                             any
}

// ResolveHome returns the active Hermes profile home. HERMES_HOME is the
// authoritative profile boundary; the default is ~/.hermes.
func ResolveHome() string {
	if home := os.Getenv("HERMES_HOME"); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hermes")
}

// ResolveUsageIn reads one exact session from a Hermes profile home.
func ResolveUsageIn(home, sessionID string) *core.ContextUsage {
	if home == "" || sessionID == "" {
		return nil
	}
	dbPath := filepath.Join(home, "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	// Read-only and query_only: the live agent owns this database, and a WAL
	// reader must never create or rewrite journal state behind it.
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		return nil
	}
	defer db.Close()

	cols := tableColumns(db, "sessions")
	if !cols["id"] {
		return nil
	}
	expr := func(name, fallback string) string {
		if cols[name] {
			return "COALESCE(" + name + ", " + fallback + ")"
		}
		return fallback
	}
	query := `SELECT ` + strings.Join([]string{
		expr("model", "''"), expr("model_config", "''"), expr("billing_provider", "''"),
		expr("billing_base_url", "''"), expr("billing_mode", "''"), expr("input_tokens", "0"),
		expr("output_tokens", "0"), expr("cache_read_tokens", "0"), expr("cache_write_tokens", "0"),
		expr("actual_cost_usd", "0"), expr("estimated_cost_usd", "0"),
	}, ", ") + ` FROM sessions WHERE id = ? LIMIT 1`
	var row sessionRow
	if err := db.QueryRow(query, sessionID).Scan(&row.model, &row.modelConfig, &row.billingProvider,
		&row.billingBaseURL, &row.billingMode, &row.input, &row.output, &row.cacheRead,
		&row.cacheWrite, &row.actualCost, &row.estimatedCost); err != nil {
		return nil
	}

	messages := activeMessages(db, sessionID)
	contextTokens := anchoredTokens(row.modelConfig, messages)
	usage := &core.ContextUsage{
		SessionTokens:   nonNegative(row.input) + nonNegative(row.cacheRead) + nonNegative(row.cacheWrite) + nonNegative(row.output),
		SessionCostUSD:  preferredCost(row.actualCost, row.estimatedCost),
		BillingProvider: backendIdentity(row.billingProvider, row.billingBaseURL),
		BillingMode:     row.billingMode,
		SessionCache:    core.CacheFromTokenCounts(nonNegative(row.input), nonNegative(row.cacheRead), nonNegative(row.cacheWrite)),
	}
	if contextTokens != nil {
		usage.ContextTokens = *contextTokens
	}
	usage.WindowTokens = contextWindow(home, row.model, row.modelConfig, row.billingBaseURL)
	if usage.ContextTokens == 0 && usage.SessionTokens == 0 && usage.SessionCache == nil && usage.BillingProvider == "" && usage.WindowTokens == nil {
		return nil
	}
	return usage
}

func tableColumns(db *sql.DB, table string) map[string]bool {
	out := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var def any
		if rows.Scan(&cid, &name, &typ, &notnull, &def, &pk) == nil {
			out[name] = true
		}
	}
	return out
}

func activeMessages(db *sql.DB, sessionID string) []message {
	cols := tableColumns(db, "messages")
	if !cols["session_id"] || !cols["role"] || !cols["content"] {
		return nil
	}
	expr := func(name string) string {
		if cols[name] {
			return "COALESCE(" + name + ", '')"
		}
		return "''"
	}
	where := "session_id = ?"
	if cols["active"] {
		where += " AND COALESCE(active, 1) = 1"
	}
	order := "rowid"
	if cols["id"] {
		order = "id"
	}
	rows, err := db.Query(`SELECT role, COALESCE(content, ''), `+expr("api_content")+`, `+expr("tool_call_id")+`, `+expr("tool_calls")+` FROM messages WHERE `+where+` ORDER BY `+order, sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []message
	for rows.Next() {
		var m message
		var toolCalls string
		if rows.Scan(&m.Role, &m.Content, &m.APIContent, &m.ToolCallID, &toolCalls) != nil {
			continue
		}
		if toolCalls != "" {
			if json.Unmarshal([]byte(toolCalls), &m.ToolCalls) != nil {
				m.ToolCalls = nil
			}
		}
		out = append(out, m)
	}
	return out
}

func anchoredTokens(config string, messages []message) *int {
	var root map[string]any
	if json.Unmarshal([]byte(config), &root) != nil {
		return nil
	}
	a, ok := root["_usage_anchor"].(map[string]any)
	if !ok {
		return nil
	}
	pt, pok := positiveJSONInt(a["prompt_tokens"])
	bc, bok := positiveJSONInt(a["base_count"])
	ct, _ := jsonInt(a["completion_tokens"])
	lastFP, fok := a["base_last_fp"].(string)
	prefixFP, xok := a["base_prefix_fp"].(string)
	lastRole, _ := a["base_last_role"].(string)
	if !pok || !bok || !fok || !xok || bc > len(messages) || bc == 0 {
		return nil
	}
	if messages[bc-1].Role != lastRole || messageFingerprint(messages[bc-1]) != lastFP || prefixFingerprint(messages[:bc]) != prefixFP {
		return nil
	}
	total := pt + max(0, ct)
	delta := messages[bc:]
	if len(delta) > 0 && delta[0].Role == "assistant" {
		delta = delta[1:]
	}
	for _, m := range delta {
		total += estimateMessage(m)
	}
	return &total
}

func fingerprintPayload(m message) map[string]any {
	p := map[string]any{"role": m.Role}
	if m.Content != "" {
		var v any
		if json.Unmarshal([]byte(m.Content), &v) == nil {
			p["content"] = v
		} else {
			p["content"] = m.Content
		}
	}
	if m.APIContent != "" {
		p["api_content"] = m.APIContent
	}
	if m.ToolCallID != "" {
		p["tool_call_id"] = m.ToolCallID
	}
	if m.ToolCalls != nil {
		p["tool_calls"] = m.ToolCalls
	}
	return p
}

func messageFingerprint(m message) string {
	raw, _ := json.Marshal(fingerprintPayload(m))
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func prefixFingerprint(messages []message) string {
	pairs := make([][2]string, 0, len(messages))
	for _, m := range messages {
		pairs = append(pairs, [2]string{m.Role, messageFingerprint(m)})
	}
	raw, _ := json.Marshal(pairs)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func estimateMessage(m message) int {
	p := fingerprintPayload(m)
	if (m.Role == "user" || m.Role == "assistant") && m.APIContent != "" {
		p["content"] = m.APIContent
		delete(p, "api_content")
	}
	raw, _ := json.Marshal(p)
	return (len(raw) + 3) / 4
}

func contextWindow(home, model, config, baseURL string) *int {
	var root map[string]any
	if json.Unmarshal([]byte(config), &root) == nil {
		if n, ok := positiveJSONInt(root["context_length"]); ok {
			return &n
		}
		if runtime, ok := root["gateway_runtime"].(map[string]any); ok {
			if n, ok := positiveJSONInt(runtime["context_length"]); ok {
				return &n
			}
			if s, ok := runtime["base_url"].(string); ok && s != "" {
				baseURL = s
			}
		}
		if s, ok := root["base_url"].(string); ok && s != "" {
			baseURL = s
		}
	}
	raw, err := os.ReadFile(filepath.Join(home, "context_length_cache.yaml"))
	if err != nil || model == "" {
		return nil
	}
	entries := parseContextCache(string(raw))
	baseURL = strings.TrimRight(baseURL, "/")
	keys := []string{model + "@" + baseURL}
	if i := strings.Index(model, "/"); i > 0 {
		keys = append(keys, model[i+1:]+"@"+baseURL)
	}
	var best *int
	for _, key := range keys {
		if n, ok := entries[key]; ok && n > 0 && (best == nil || n < *best) {
			v := n
			best = &v
		}
	}
	return best
}

func parseContextCache(raw string) map[string]int {
	out := map[string]int{}
	in := false
	for _, line := range strings.Split(raw, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "context_lengths:" {
			in = true
			continue
		}
		if !in || trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if len(line) == len(strings.TrimLeft(line, " \t")) {
			break
		}
		i := strings.LastIndex(trim, ":")
		if i < 0 {
			continue
		}
		key := strings.Trim(strings.TrimSpace(trim[:i]), "'\"")
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(trim[i+1:]), "%d", &n); err == nil {
			out[key] = n
		}
	}
	return out
}

// backendIdentity names the billed backend. The recorded billing provider is
// authoritative (it preserves gateway identities such as llm-rosetta); the
// endpoint host is only a fallback label.
func backendIdentity(billingProvider, baseURL string) string {
	if billingProvider != "" {
		return billingProvider
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "api.")
	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return host
}

func preferredCost(actual, estimated float64) float64 {
	if actual > 0 {
		return actual
	}
	if estimated > 0 {
		return estimated
	}
	return 0
}
func nonNegative(n int) int {
	if n > 0 {
		return n
	}
	return 0
}
func jsonInt(v any) (int, bool)         { n, ok := v.(float64); return int(n), ok }
func positiveJSONInt(v any) (int, bool) { n, ok := jsonInt(v); return n, ok && n > 0 }
