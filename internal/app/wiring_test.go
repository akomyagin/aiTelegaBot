// Wiring-тест Этапа 9A (план, тест-кейс 7): выбор реализации вызова A по
// конфигу — offline-cfg → *llm.Offline (без сети/httptest-вызовов), иначе
// *llm.Client. Pipeline.Classify получает результат newSummarizer напрямую
// (app.go), поэтому проверка выбора покрывает wiring целиком.

package app

import (
	"context"
	"testing"

	"github.com/akomyagin/aiTelegaBot/internal/config"
	"github.com/akomyagin/aiTelegaBot/internal/llm"
)

func TestNewSummarizer_OfflineConfig(t *testing.T) {
	s := newSummarizer(&config.Config{Offline: true})
	off, ok := s.(*llm.Offline)
	if !ok {
		t.Fatalf("newSummarizer(offline) = %T, want *llm.Offline", s)
	}
	// Без сети: ClassifySummarize работает без единого httptest-сервера —
	// детерминированная деградация (class=other, Degraded=true).
	res, err := off.ClassifySummarize(context.Background(),
		[]llm.ClassifyItem{{Title: "Заголовок", Text: "Ведущее предложение. Хвост текста."}})
	if err != nil {
		t.Fatalf("ClassifySummarize: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Class != llm.ClassOther {
		t.Fatalf("res.Items = %+v, want 1×other", res.Items)
	}
	if len(res.Degraded) != 1 || !res.Degraded[0] {
		t.Errorf("res.Degraded = %v, want [true]", res.Degraded)
	}
}

func TestNewSummarizer_ClientConfig(t *testing.T) {
	s := newSummarizer(&config.Config{
		LLMAPIKey:     "test-key",
		LLMBaseURL:    "http://127.0.0.1:1/v1", // недостижим; конструирование без сети
		LLMModel:      "test-model",
		LLMMaxRetries: 2,
	})
	if _, ok := s.(*llm.Client); !ok {
		t.Fatalf("newSummarizer(client) = %T, want *llm.Client", s)
	}
}

// --- Wiring шага 4c (Этап 9B): newLinkStack по конфигу ---

func TestNewLinkStack_OfflineConfig(t *testing.T) {
	e, f, fetch := newLinkStack(&config.Config{Offline: true})
	if _, ok := e.(*llm.Offline); !ok {
		t.Errorf("LinkClassifier = %T, want *llm.Offline", e)
	}
	if _, ok := f.(*llm.Offline); !ok {
		t.Errorf("LinkSummarizer = %T, want *llm.Offline", f)
	}
	if fetch != nil {
		t.Errorf("offline LinkFetch = %+v, want nil (fetch-фаза пропускается)", fetch)
	}
}

func TestNewLinkStack_ClientConfig(t *testing.T) {
	e, f, fetch := newLinkStack(&config.Config{
		LLMAPIKey:     "test-key",
		LLMBaseURL:    "http://127.0.0.1:1/v1", // недостижим; конструирование без сети
		LLMModel:      "test-model",
		LLMMaxRetries: 2,
		// LinkFetchTimeout=0 — проверка страховки дефолта (Load даёт 15s);
		// fetch не выполняется, сеть не нужна.
	})
	if _, ok := e.(*llm.Client); !ok {
		t.Errorf("LinkClassifier = %T, want *llm.Client", e)
	}
	if _, ok := f.(*llm.Client); !ok {
		t.Errorf("LinkSummarizer = %T, want *llm.Client", f)
	}
	if fetch == nil {
		t.Error("LinkFetch = nil, want fetcher in online mode")
	}
}
