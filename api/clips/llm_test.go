package clips

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Ollama provider
// ---------------------------------------------------------------------------

func TestGenerateSummaryWithLLM_Ollama_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/generate") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"response": "A short summary."})
	}))
	defer srv.Close()

	t.Setenv("LLM_PROVIDER", "ollama")
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_MODEL", "test-model")

	text, model, err := GenerateSummaryWithLLM("summarise this")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "test-model" {
		t.Errorf("model = %q, want 'test-model'", model)
	}
	if text != "A short summary." {
		t.Errorf("text = %q, want 'A short summary.'", text)
	}
}

func TestGenerateSummaryWithLLM_Ollama_LeadingTrailingSpaceTrimmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"response": "  trimmed  "})
	}))
	defer srv.Close()

	t.Setenv("LLM_PROVIDER", "ollama")
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_MODEL", "m")

	text, _, err := GenerateSummaryWithLLM("p")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "trimmed" {
		t.Errorf("text = %q, want 'trimmed'", text)
	}
}

func TestGenerateSummaryWithLLM_Ollama_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	t.Setenv("LLM_PROVIDER", "ollama")
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_MODEL", "m")

	_, _, err := GenerateSummaryWithLLM("p")
	if err == nil {
		t.Fatal("expected error on HTTP 503, got nil")
	}
}

// ---------------------------------------------------------------------------
// Anthropic provider
// ---------------------------------------------------------------------------

func TestGenerateSummaryWithLLM_Anthropic_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-api-key") != "testkey" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"content": []map[string]string{
				{"type": "text", "text": "Anthropic summary."},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_MODEL", "claude-haiku")
	t.Setenv("LLM_API_KEY", "testkey")

	text, model, err := GenerateSummaryWithLLM("summarise this")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "claude-haiku" {
		t.Errorf("model = %q, want 'claude-haiku'", model)
	}
	if text != "Anthropic summary." {
		t.Errorf("text = %q, want 'Anthropic summary.'", text)
	}
}

func TestGenerateSummaryWithLLM_Anthropic_MultipleContentBlocks(t *testing.T) {
	// Multiple "text" blocks should be joined with a space.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"content": []map[string]string{
				{"type": "text", "text": "Part one."},
				{"type": "text", "text": "Part two."},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_MODEL", "m")
	t.Setenv("LLM_API_KEY", "k")

	text, _, err := GenerateSummaryWithLLM("p")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "Part one. Part two." {
		t.Errorf("text = %q, want 'Part one. Part two.'", text)
	}
}

func TestGenerateSummaryWithLLM_Anthropic_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_MODEL", "m")
	t.Setenv("LLM_API_KEY", "k")

	_, _, err := GenerateSummaryWithLLM("p")
	if err == nil {
		t.Fatal("expected error on HTTP 429, got nil")
	}
}

func TestGenerateSummaryWithLLM_Anthropic_MissingKey(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "anthropic")
	t.Setenv("LLM_BASE_URL", "http://127.0.0.1:0")
	t.Setenv("LLM_MODEL", "m")
	t.Setenv("LLM_API_KEY", "")

	_, _, err := GenerateSummaryWithLLM("p")
	if err == nil {
		t.Fatal("expected error for missing API key, got nil")
	}
	if !strings.Contains(err.Error(), "missing API key") {
		t.Errorf("error = %q, want to contain 'missing API key'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// OpenAI-compatible provider
// ---------------------------------------------------------------------------

func TestGenerateSummaryWithLLM_OpenAI_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer openai-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Content is a bare JSON string.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]interface{}{"content": "OpenAI summary."}},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_MODEL", "gpt-4o-mini")
	t.Setenv("LLM_API_KEY", "openai-key")

	text, model, err := GenerateSummaryWithLLM("summarise this")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if model != "gpt-4o-mini" {
		t.Errorf("model = %q, want 'gpt-4o-mini'", model)
	}
	if text != "OpenAI summary." {
		t.Errorf("text = %q, want 'OpenAI summary.'", text)
	}
}

func TestGenerateSummaryWithLLM_OpenAI_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{}})
	}))
	defer srv.Close()

	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_MODEL", "m")
	t.Setenv("LLM_API_KEY", "k")

	text, _, err := GenerateSummaryWithLLM("p")
	if err != nil {
		t.Fatalf("unexpected error for 0 choices: %v", err)
	}
	if text != "" {
		t.Errorf("text = %q, want '' for 0 choices", text)
	}
}

func TestGenerateSummaryWithLLM_OpenAI_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_BASE_URL", srv.URL)
	t.Setenv("LLM_MODEL", "m")
	t.Setenv("LLM_API_KEY", "k")

	_, _, err := GenerateSummaryWithLLM("p")
	if err == nil {
		t.Fatal("expected error on HTTP 500, got nil")
	}
}

func TestGenerateSummaryWithLLM_OpenAI_MissingKey(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_BASE_URL", "http://127.0.0.1:0")
	t.Setenv("LLM_MODEL", "m")
	t.Setenv("LLM_API_KEY", "")

	_, _, err := GenerateSummaryWithLLM("p")
	if err == nil {
		t.Fatal("expected error for missing API key, got nil")
	}
}
