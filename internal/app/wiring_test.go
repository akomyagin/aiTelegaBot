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
