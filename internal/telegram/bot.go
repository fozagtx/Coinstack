// Package telegram is the CoinStack Telegram bot: it answers commands from
// authorised chats, sends discovery alerts on each new market snapshot and
// posts a daily digest. It speaks plain Bot API over net/http and reuses
// internal/discover for every computation. All state is in memory: a
// restart silently re-seeds the seen sets.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fozagtx/coinstack/internal/discover"
	"github.com/fozagtx/coinstack/internal/market"
)

const (
	defaultBaseURL = "https://api.telegram.org"
	apiTimeout     = 45 * time.Second // covers the 30 s getUpdates long-poll
	sendGap        = time.Second      // minimum spacing between outgoing messages
	maxMessageLen  = 4000
	signoff        = "\n\n<i>Not financial advice.</i>"
)

// Options configures the bot.
type Options struct {
	// Token is the Bot API token; required.
	Token string
	// ChatIDs are the alert targets and the only chats allowed to run
	// commands.
	ChatIDs []int64
	// DigestHour is the UTC hour the daily digest fires; default 9.
	DigestHour int
	// Alerts selects which alert kinds run; empty means all of
	// gems, climbers, listings, digest.
	Alerts []string
	// Engine answers the discovery queries; required.
	Engine *discover.Engine
	// Subscribe registers the alert evaluator for each new snapshot;
	// typically market.Market.Subscribe.
	Subscribe func(fn func(*market.Snapshot))
	// BaseURL overrides the Bot API origin; default https://api.telegram.org.
	BaseURL string
	// HTTPClient defaults to a client with a 45 s timeout.
	HTTPClient *http.Client
	// Now defaults to time.Now; for tests.
	Now func() time.Time
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Bot is the Telegram bot. Create with New, start with Run.
type Bot struct {
	opts   Options
	engine *discover.Engine
	log    *slog.Logger
	now    func() time.Time
	hc     *http.Client
	api    string

	outbound chan outbound

	mu           sync.Mutex
	lastUpdateAt *time.Time
	alertsSent   atomic.Int64

	// alert state
	seeded       bool // first snapshot has been consumed without alerting
	gemSeen      map[int64]time.Time
	climbSeen    map[int64]time.Time
	listingSeen  map[int64]bool
	lastDigestAt time.Time
}

type outbound struct {
	chatID int64
	text   string
	alert  bool
}

// New validates opts and returns the bot.
func New(opts Options) *Bot {
	if opts.Token == "" {
		panic("telegram: Token is required")
	}
	if opts.Engine == nil {
		panic("telegram: Engine is required")
	}
	if opts.DigestHour < 0 || opts.DigestHour > 23 {
		opts.DigestHour = 9
	}
	if len(opts.Alerts) == 0 {
		opts.Alerts = []string{"gems", "climbers", "listings", "digest"}
	}
	if opts.BaseURL == "" {
		opts.BaseURL = defaultBaseURL
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: apiTimeout}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Bot{
		opts:        opts,
		engine:      opts.Engine,
		log:         opts.Logger.With("component", "telegram"),
		now:         opts.Now,
		hc:          opts.HTTPClient,
		api:         opts.BaseURL + "/bot" + opts.Token,
		outbound:    make(chan outbound, 64),
		gemSeen:     map[int64]time.Time{},
		climbSeen:   map[int64]time.Time{},
		listingSeen: map[int64]bool{},
	}
}

// Status reports bot health for /v1/health.
func (b *Bot) Status() (enabled bool, chats int, lastUpdateAt *time.Time, alertsSent int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return true, len(b.opts.ChatIDs), b.lastUpdateAt, b.alertsSent.Load()
}

func (b *Bot) alertOn(kind string) bool { return slices.Contains(b.opts.Alerts, kind) }

// Run starts the update loop, the alert evaluator and the digest ticker,
// and blocks until ctx is done.
func (b *Bot) Run(ctx context.Context) error {
	go b.sendLoop(ctx)
	if b.opts.Subscribe != nil {
		b.opts.Subscribe(b.evaluate)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.updatesLoop(ctx) }()
	go func() { defer wg.Done(); b.digestLoop(ctx) }()
	<-ctx.Done()
	wg.Wait()
	return nil
}

// --- Telegram API plumbing ---

type tgResponse struct {
	OK         bool            `json:"ok"`
	Result     json.RawMessage `json:"result"`
	ErrorCode  int             `json:"error_code"`
	Parameters struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
	Description string `json:"description"`
}

// call invokes a Bot API method and returns the result payload.
func (b *Bot) call(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.api+"/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var tr tgResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("telegram %s: bad response: %w", method, err)
	}
	if !tr.OK {
		return nil, &rateError{code: tr.ErrorCode, retryAfter: tr.Parameters.RetryAfter, desc: tr.Description}
	}
	return tr.Result, nil
}

// rateError is a Bot API error; RetryAfter is set on 429.
type rateError struct {
	code       int
	retryAfter int
	desc       string
}

func (e *rateError) Error() string {
	return fmt.Sprintf("telegram api error %d: %s", e.code, e.desc)
}

// send queues a message to one chat; it never blocks for long.
func (b *Bot) send(chatID int64, text string, alert bool) {
	if len(text) > maxMessageLen {
		text = text[:maxMessageLen-20] + "\n[truncated]"
	}
	select {
	case b.outbound <- outbound{chatID: chatID, text: text, alert: alert}:
	default:
		b.log.Warn("telegram outbound queue full; dropping message", "chat", chatID)
	}
}

// broadcast queues a message for every authorised chat.
func (b *Bot) broadcast(text string) {
	for _, id := range b.opts.ChatIDs {
		b.send(id, text, true)
	}
}

// sendLoop delivers queued messages, spacing them at least sendGap apart
// and honoring Telegram's 429 retry_after.
func (b *Bot) sendLoop(ctx context.Context) {
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-b.outbound:
			if wait := sendGap - time.Since(last); wait > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(wait):
				}
			}
			last = time.Now()
			b.deliver(ctx, m)
		}
	}
}

func (b *Bot) deliver(ctx context.Context, m outbound) {
	payload := map[string]any{"chat_id": m.chatID, "text": m.text, "parse_mode": "HTML", "disable_web_page_preview": true}
	_, err := b.call(ctx, "sendMessage", payload)
	var re *rateError
	switch {
	case err == nil:
		if m.alert {
			b.alertsSent.Add(1)
		}
	case errors.As(err, &re) && re.code == http.StatusTooManyRequests && re.retryAfter > 0:
		wait := time.Duration(re.retryAfter) * time.Second
		b.log.Warn("telegram rate limited", "retry_after", wait)
		select {
		case <-ctx.Done():
		case <-time.After(wait):
			b.deliver(ctx, m)
		}
	case err != nil:
		b.log.Warn("sendMessage failed", "chat", m.chatID, "err", err)
	}
}

// --- update polling ---

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgMessage struct {
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	Text string `json:"text"`
}

// updatesLoop long-polls getUpdates and dispatches commands.
func (b *Bot) updatesLoop(ctx context.Context) {
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		raw, err := b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 30})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.log.Warn("getUpdates failed", "err", err, "retry_in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		var updates []tgUpdate
		if err := json.Unmarshal(raw, &updates); err != nil {
			b.log.Warn("getUpdates: bad payload", "err", err)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			now := b.now()
			b.mu.Lock()
			b.lastUpdateAt = &now
			b.mu.Unlock()
			if u.Message != nil {
				b.handleMessage(u.Message)
			}
		}
	}
}

// handleMessage dispatches a bot command.
func (b *Bot) handleMessage(m *tgMessage) {
	if m.Text == "" || m.Text[0] != '/' {
		return
	}
	chat := m.Chat.ID
	if !slices.Contains(b.opts.ChatIDs, chat) {
		b.send(chat, fmt.Sprintf("Not authorised. Your chat id is <code>%d</code> &mdash; add it to TELEGRAM_CHAT_IDS.", chat), false)
		return
	}
	text, _ := m.Text, 0
	// strip the @botname suffix Telegram appends to commands
	cmd, arg := text, ""
	if i := indexByte(text, ' '); i >= 0 {
		cmd, arg = text[:i], text[i+1:]
	}
	if i := indexByte(cmd, '@'); i >= 0 {
		cmd = cmd[:i]
	}
	reply, err := b.command(context.Background(), cmd, arg)
	if err != nil {
		reply = "Error: " + esc(err.Error())
	}
	if reply == "" {
		reply = "Unknown command. Try /help."
	}
	b.send(chat, reply+signoff, false)
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
