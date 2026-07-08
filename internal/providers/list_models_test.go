package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestOpenAICompatListModels(t *testing.T) {
	var gotAuth, gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{
				{"id": "gpt-b"}, {"id": "gpt-a"}, {"id": "gpt-b"},
			},
		})
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "sk-test")
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if want := []string{"gpt-a", "gpt-b"}; !reflect.DeepEqual(models, want) {
		t.Errorf("models = %v, want %v (sorted, deduped)", models, want)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want Bearer sk-test", gotAuth)
	}
	if gotPath != "/models" {
		t.Errorf("path = %q, want /models", gotPath)
	}
}

func TestOpenAICompatListModelsNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusUnauthorized)
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "bad")
	_, err := p.ListModels(context.Background())
	perr, ok := err.(*Error)
	if !ok {
		t.Fatalf("err = %T (%v), want *Error", err, err)
	}
	if perr.Status != http.StatusUnauthorized {
		t.Errorf("Status = %d, want 401", perr.Status)
	}
}

func TestOpenAICompatListModelsBadJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer ts.Close()

	p := NewOpenAICompat("up", "openai", ts.URL, "")
	if _, err := p.ListModels(context.Background()); err == nil {
		t.Fatal("malformed JSON must return an error")
	}
}

func TestMockListModels(t *testing.T) {
	m := NewMock("mock")
	models, err := m.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"mock-gpt", "mock-large", "mock-small"}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("models = %v, want %v", models, want)
	}
}

// Compile-time capability checks.
var (
	_ ModelLister = (*OpenAICompat)(nil)
	_ ModelLister = (*Mock)(nil)
)
