// Package rates — курсы валют для пересчёта итогов в основную валюту.
package rates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"
)

// DefaultURL — бесплатный источник без ключа; курсы относительно USD.
const DefaultURL = "https://open.er-api.com/v6/latest/USD"

type Service struct {
	url    string
	client *http.Client
	ttl    time.Duration
	retry  time.Duration

	mu      sync.Mutex
	perUSD  map[string]float64
	updated time.Time // время обновления курсов у провайдера
	fetched time.Time
	tried   time.Time
}

// New создаёт сервис. Пустой url отключает загрузку курсов.
func New(url string) *Service {
	return &Service{
		url:    url,
		client: &http.Client{Timeout: 10 * time.Second},
		ttl:    6 * time.Hour,
		retry:  5 * time.Minute,
	}
}

// Rate возвращает курс: сколько единиц to за одну единицу from.
func (s *Service) Rate(ctx context.Context, from, to string) (float64, bool) {
	if from == to {
		return 1, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensure(ctx)
	f, ok1 := s.lookup(from)
	t, ok2 := s.lookup(to)
	if !ok1 || !ok2 || f <= 0 {
		return 0, false
	}
	return t / f, true
}

// Convert пересчитывает сумму в сотых долях.
func (s *Service) Convert(ctx context.Context, amount int64, from, to string) (int64, bool) {
	r, ok := s.Rate(ctx, from, to)
	if !ok {
		return 0, false
	}
	return int64(math.Round(float64(amount) * r)), true
}

// Updated — время актуальности курсов (нулевое, если курсов нет).
func (s *Service) Updated() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updated
}

func (s *Service) lookup(code string) (float64, bool) {
	if v, ok := s.perUSD[code]; ok {
		return v, true
	}
	// Стейблкоины считаем равными доллару.
	if code == "USDT" || code == "USDC" || code == "USD" {
		if s.perUSD != nil {
			return 1, true
		}
	}
	return 0, false
}

func (s *Service) ensure(ctx context.Context) {
	if s.url == "" {
		return
	}
	now := time.Now()
	if s.perUSD != nil && now.Sub(s.fetched) < s.ttl {
		return
	}
	if now.Sub(s.tried) < s.retry {
		return
	}
	s.tried = now
	rates, updated, err := s.fetch(ctx)
	if err != nil {
		slog.Warn("не удалось обновить курсы валют", "err", err)
		return
	}
	s.perUSD, s.updated, s.fetched = rates, updated, now
}

func (s *Service) fetch(ctx context.Context) (map[string]float64, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Result         string             `json:"result"`
		TimeLastUpdate int64              `json:"time_last_update_unix"`
		Rates          map[string]float64 `json:"rates"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, time.Time{}, err
	}
	if body.Result != "success" || len(body.Rates) == 0 {
		return nil, time.Time{}, fmt.Errorf("неожиданный ответ: result=%q", body.Result)
	}
	for code, v := range body.Rates {
		if v <= 0 || math.IsInf(v, 0) || math.IsNaN(v) {
			delete(body.Rates, code)
		}
	}
	updated := time.Now()
	if body.TimeLastUpdate > 0 {
		updated = time.Unix(body.TimeLastUpdate, 0)
	}
	return body.Rates, updated, nil
}
