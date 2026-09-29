package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fozagtx/coinstack/internal/discover"
	"github.com/fozagtx/coinstack/internal/market"
	"github.com/fozagtx/coinstack/internal/model"
)

var testNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

type fakeMarket struct {
	snap    *market.Snapshot
	hist    map[int64][]model.Sample
	newList []model.Quote
	quotes  map[int64]model.Quote
	hours   int
}

func (f *fakeMarket) Snapshot() *market.Snapshot { return f.snap }
func (f *fakeMarket) Quotes(context.Context, []int64) (map[int64]model.Quote, error) {
	return f.quotes, nil
}
func (f *fakeMarket) Info(context.Context, []int64) (map[int64]model.Info, error) { return nil, nil }
func (f *fakeMarket) NewListings(context.Context, int) ([]model.Quote, error)     { return f.newList, nil }
func (f *fakeMarket) History(id int64) []model.Sample                             { return f.hist[id] }
func (f *fakeMarket) RankAt(id int64, ago time.Duration) (model.Sample, bool) {
	if r := f.hist[id]; len(r) > 0 {
		return r[0], true
	}
	return model.Sample{}, false
}
func (f *fakeMarket) HistoryHours() int { return f.hours }
func (f *fakeMarket) Status() model.MarketStatus {
	return model.MarketStatus{TopN: 3000, CacheSize: f.snap.Len(), LastSuccessAt: testNow, PollInterval: 2 * time.Minute}
}

func mkQuote(id int64, sym string, rank int, mcap, vol, c24 float64, tags ...string) model.Quote {
	return model.Quote{
		ID: id, Symbol: sym, Name: sym + " name", Slug: sym,
		Rank: rank, Price: 1, MarketCap: mcap, Volume24h: vol, Change24hPct: c24,
		DateAdded: testNow.Add(-10 * 24 * time.Hour), Tags: tags,
		LastUpdated: testNow, FetchedAt: testNow,
	}
}

// tgStub emulates api.telegram.org.
type tgStub struct {
	mu       sync.Mutex
	updates  []tgUpdate
	sent     []map[string]any
	sentCh   chan map[string]any
	polledAt int
}

func newTgStub() *tgStub {
	return &tgStub{sentCh: make(chan map[string]any, 64)}
}

func (s *tgStub) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /botT/getUpdates", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		up := s.updates
		s.updates = nil
		s.polledAt++
		s.mu.Unlock()
		res, _ := json.Marshal(up)
		fmt.Fprintf(w, `{"ok":true,"result":%s}`, res)
	})
	mux.HandleFunc("POST /botT/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		s.mu.Lock()
		s.sent = append(s.sent, m)
		s.mu.Unlock()
		select {
		case s.sentCh <- m:
		default:
		}
		fmt.Fprint(w, `{"ok":true,"result":{}}`)
	})
	return mux
}

func (s *tgStub) pushUpdate(id int64, chat int64, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, tgUpdate{
		UpdateID: id,
		Message:  &tgMessage{Text: text},
	})
	s.updates[len(s.updates)-1].Message.Chat.ID = chat
}

func (s *tgStub) waitSent(t *testing.T) map[string]any {
	t.Helper()
	select {
	case m := <-s.sentCh:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no sendMessage call")
		return nil
	}
}

func (s *tgStub) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func newBot(t *testing.T, stub *tgStub, m *fakeMarket) *Bot {
	t.Helper()
	engine := discover.New(m, nil, func() time.Time { return testNow })
	return New(Options{
		Token:      "T",
		ChatIDs:    []int64{42},
		DigestHour: 9,
		Engine:     engine,
		BaseURL:    "", // set below
		Now:        func() time.Time { return testNow },
	})
}

func TestGemsCommand(t *testing.T) {
	stub := newTgStub()
	m := &fakeMarket{snap: market.NewSnapshot([]model.Quote{
		mkQuote(1, "GEMX", 500, 20e6, 4e6, 10, "depin"),
	}, testNow), hours: 100}
	stub.pushUpdate(1, 42, "/gems")
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()

	engine := discover.New(m, nil, func() time.Time { return testNow })
	bot := New(Options{Token: "T", ChatIDs: []int64{42}, Engine: engine,
		BaseURL: srv.URL, Now: func() time.Time { return testNow },
		HTTPClient: srv.Client()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bot.Run(ctx)

	msg := stub.waitSent(t)
	text, _ := msg["text"].(string)
	if got := fmt.Sprint(msg["chat_id"]); got != "42" {
		t.Fatalf("chat_id %s", got)
	}
	if !contains(text, "GEMX") {
		t.Fatalf("gems reply missing symbol: %s", text)
	}
	if !contains(text, "Not financial advice") {
		t.Fatalf("missing signoff: %s", text)
	}
}

func TestUnauthorisedChatGetsHint(t *testing.T) {
	stub := newTgStub()
	m := &fakeMarket{snap: market.NewSnapshot(nil, testNow)}
	stub.pushUpdate(1, 99, "/gems")
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	engine := discover.New(m, nil, func() time.Time { return testNow })
	bot := New(Options{Token: "T", ChatIDs: []int64{42}, Engine: engine,
		BaseURL: srv.URL, Now: func() time.Time { return testNow }, HTTPClient: srv.Client()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go bot.Run(ctx)
	msg := stub.waitSent(t)
	text, _ := msg["text"].(string)
	if !contains(text, "99") || !contains(text, "TELEGRAM_CHAT_IDS") {
		t.Fatalf("want chat-id hint, got %s", text)
	}
}

func TestGemAlertSeedsThenFires(t *testing.T) {
	m := &fakeMarket{snap: market.NewSnapshot([]model.Quote{
		mkQuote(1, "GEMX", 500, 20e6, 4e6, 10, "depin"),
	}, testNow), hours: 100}
	stub := newTgStub()
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	engine := discover.New(m, nil, func() time.Time { return testNow })
	bot := New(Options{Token: "T", ChatIDs: []int64{42}, Engine: engine,
		BaseURL: srv.URL, Now: func() time.Time { return testNow }, HTTPClient: srv.Client()})
	go bot.sendLoop(context.Background())

	snap := m.snap
	bot.evaluate(snap) // first snapshot seeds silently
	if n := stub.sentCount(); n != 0 {
		t.Fatalf("first snapshot must not alert, got %d messages", n)
	}

	// add a new gem; second evaluate alerts once
	m.snap = market.NewSnapshot([]model.Quote{
		mkQuote(1, "GEMX", 500, 20e6, 4e6, 10, "depin"),
		mkQuote(2, "FRESH", 600, 15e6, 3e6, 8, "depin"),
	}, testNow)
	bot.evaluate(m.snap)
	msg := stub.waitSent(t)
	if !contains(msg["text"].(string), "FRESH") {
		t.Fatalf("alert should name FRESH: %s", msg["text"])
	}

	// repeat within cooldown: no new alert
	bot.evaluate(m.snap)
	select {
	case m2 := <-stub.sentCh:
		t.Fatalf("repeat should be cooled down, got %v", m2["text"])
	case <-time.After(200 * time.Millisecond):
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
