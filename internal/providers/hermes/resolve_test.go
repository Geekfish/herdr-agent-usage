package hermes

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/senna-lang/herdr-agent-usage/internal/provider"
	_ "modernc.org/sqlite"
)

func openFixture(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sessions (
 id TEXT PRIMARY KEY, cwd TEXT, model TEXT, model_config TEXT,
 input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER,
 cache_write_tokens INTEGER, reasoning_tokens INTEGER, billing_provider TEXT,
 billing_base_url TEXT, billing_mode TEXT, estimated_cost_usd REAL,
 actual_cost_usd REAL);
CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT,
 content TEXT, api_content TEXT, tool_call_id TEXT, tool_calls TEXT, active INTEGER);`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func anchorConfig(t *testing.T, messages []message, prompt, completion int) string {
	t.Helper()
	a := map[string]any{
		"prompt_tokens": prompt, "completion_tokens": completion,
		"base_count": len(messages), "base_last_role": messages[len(messages)-1].Role,
		"base_last_fp":   messageFingerprint(messages[len(messages)-1]),
		"base_prefix_fp": prefixFingerprint(messages),
	}
	raw, err := json.Marshal(map[string]any{"_usage_anchor": a})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestResolveUsageInRequiresExactSessionID(t *testing.T) {
	home := t.TempDir()
	db := openFixture(t, home)
	_, err := db.Exec(`INSERT INTO sessions (id,cwd,model,model_config,input_tokens,output_tokens) VALUES
 ('wanted','/same','model-a','{}',111,22), ('other','/same','model-a','{}',999,99)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	got := ResolveUsageIn(home, "wanted")
	if got == nil || got.SessionTokens != 133 {
		t.Fatalf("exact session usage = %#v, want 133", got)
	}
	if got := ResolveUsageIn(home, "missing"); got != nil {
		t.Fatalf("missing = %#v, want nil", got)
	}
}

func TestResolveUsageInReadsWALWithoutWriting(t *testing.T) {
	home := t.TempDir()
	db := openFixture(t, home)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id,model,model_config,input_tokens) VALUES ('wal','m','{}',7)`); err != nil {
		t.Fatal(err)
	}
	got := ResolveUsageIn(home, "wal")
	if got == nil || got.SessionTokens != 7 {
		t.Fatalf("WAL usage = %#v", got)
	}
	db.Close()
}

func TestResolveUsageInToleratesOptionalColumns(t *testing.T) {
	home := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, model TEXT, model_config TEXT, input_tokens INTEGER, output_tokens INTEGER);
CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, content TEXT);
INSERT INTO sessions VALUES ('old','m','{}',8,3)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	got := ResolveUsageIn(home, "old")
	if got == nil || got.SessionTokens != 11 {
		t.Fatalf("old schema = %#v", got)
	}
}

func TestAnchoredContextUsesOnlyActiveMatchingPrefixAndDelta(t *testing.T) {
	home := t.TempDir()
	db := openFixture(t, home)
	base := []message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "answer"}}
	cfg := anchorConfig(t, base, 100, 5)
	_, err := db.Exec(`INSERT INTO sessions (id,model,model_config,input_tokens,output_tokens) VALUES ('s','m',?,900,100);
INSERT INTO messages (session_id,role,content,active) VALUES
 ('s','user','hello',1),('s','assistant','answer',1),('s','assistant','priced reply',1),('s','user','small delta',1),('s','user','inactive rewrite',0)`, cfg)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	got := ResolveUsageIn(home, "s")
	if got == nil || got.ContextTokens <= 105 || got.ContextTokens >= 130 {
		t.Fatalf("anchored context = %#v", got)
	}
	if got.ContextTokens == got.SessionTokens {
		t.Fatal("context must not be lifetime total")
	}
}

func TestStaleAnchorDoesNotUseLifetimeAsContext(t *testing.T) {
	home := t.TempDir()
	db := openFixture(t, home)
	base := []message{{Role: "user", Content: "original"}}
	cfg := anchorConfig(t, base, 100, 5)
	_, err := db.Exec(`INSERT INTO sessions (id,model,model_config,input_tokens,output_tokens) VALUES ('s','m',?,900,100);
INSERT INTO messages (session_id,role,content,active) VALUES ('s','user','changed',1)`, cfg)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	got := ResolveUsageIn(home, "s")
	if got == nil || got.ContextTokens != 0 || got.SessionTokens != 1000 {
		t.Fatalf("stale anchor = %#v", got)
	}
}

func TestUsageFieldsAndWindowResolution(t *testing.T) {
	home := t.TempDir()
	db := openFixture(t, home)
	_, err := db.Exec(`INSERT INTO sessions (id,model,model_config,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,billing_provider,billing_base_url,billing_mode,estimated_cost_usd,actual_cost_usd)
VALUES ('s','openai/model-x','{"base_url":"https://gateway.example/v1"}',100,40,300,100,25,'llm-rosetta','https://ignored.example/v1','chat_completions',1.25,2.5)`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.WriteFile(filepath.Join(home, "context_length_cache.yaml"), []byte("context_lengths:\n  openai/model-x@https://gateway.example/v1: 200000\n  model-x@https://gateway.example/v1: 180000\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := ResolveUsageIn(home, "s")
	if got == nil {
		t.Fatal("nil usage")
	}
	if got.SessionTokens != 540 {
		t.Fatalf("tokens=%d; reasoning was double-counted", got.SessionTokens)
	}
	if got.SessionCostUSD != 2.5 || got.BillingProvider != "llm-rosetta" || got.BillingMode != "chat_completions" {
		t.Fatalf("billing=%#v", got)
	}
	if got.WindowTokens == nil || *got.WindowTokens != 180000 {
		t.Fatalf("conservative alias window=%v", got.WindowTokens)
	}
	if got.SessionCache == nil || got.SessionCache.HitPercent != 60 {
		t.Fatalf("cache=%#v", got.SessionCache)
	}
}

func TestExplicitAndUnknownContextWindows(t *testing.T) {
	home := t.TempDir()
	db := openFixture(t, home)
	_, _ = db.Exec(`INSERT INTO sessions (id,model,model_config,input_tokens) VALUES ('explicit','x','{"context_length":12345}',1),('unknown','y','{}',2)`)
	db.Close()
	if got := ResolveUsageIn(home, "explicit"); got.WindowTokens == nil || *got.WindowTokens != 12345 {
		t.Fatalf("explicit=%#v", got)
	}
	if got := ResolveUsageIn(home, "unknown"); got == nil || got.WindowTokens != nil {
		t.Fatalf("unknown=%#v", got)
	}
}

func TestProfileIsolationAndProviderSessionKind(t *testing.T) {
	homeA, homeB := t.TempDir(), t.TempDir()
	for _, tc := range []struct {
		home string
		n    int
	}{{homeA, 3}, {homeB, 9}} {
		db := openFixture(t, tc.home)
		_, _ = db.Exec(`INSERT INTO sessions (id,model,model_config,input_tokens) VALUES ('same','m','{}',?)`, tc.n)
		db.Close()
	}
	t.Setenv("HERMES_HOME", homeB)
	got := Provider.ResolveUsage(provider.UsageResolveInput{Session: &provider.AgentSession{Kind: "id", Value: "same"}})
	if got == nil || got.SessionTokens != 9 {
		t.Fatalf("profile result=%#v", got)
	}
	if got := Provider.ResolveUsage(provider.UsageResolveInput{Session: &provider.AgentSession{Kind: "path", Value: "same"}}); got != nil {
		t.Fatalf("non-id=%#v", got)
	}
}

func TestCostAndBaseURLFallback(t *testing.T) {
	home := t.TempDir()
	db := openFixture(t, home)
	_, _ = db.Exec(`INSERT INTO sessions (id,model,model_config,input_tokens,billing_base_url,billing_mode,estimated_cost_usd) VALUES ('s','m','{}',1,'https://api.deepseek.com/v1','chat_completions',0.75)`)
	db.Close()
	got := ResolveUsageIn(home, "s")
	if got.SessionCostUSD != .75 || got.BillingProvider != "deepseek" {
		t.Fatalf("fallback=%#v", got)
	}
}
