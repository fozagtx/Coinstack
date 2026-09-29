package cmc

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/fozagtx/coinstack/internal/model"
)

// Options configures a Client. Zero fields take the documented defaults.
type Options struct {
	// BaseURL is the CMC Pro API root; default pro-api.coinmarketcap.com.
	BaseURL string
	// APIKey is sent as X-CMC_PRO_API_KEY.
	APIKey string
	// RequestsPerMinute caps outgoing calls; default 25.
	RequestsPerMinute int
	// HTTPClient is used for requests; default http.DefaultClient.
	HTTPClient *http.Client
	// Logger receives client logs; default slog.Default().
	Logger *slog.Logger
}

// Client is a CMC Pro API client implementing Upstream.
type Client struct {
	base    string
	key     string
	http    *http.Client
	limiter *rate.Limiter
	log     *slog.Logger
}

// New returns a Client for the CMC Pro API.
func New(o Options) *Client {
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = "https://pro-api.coinmarketcap.com"
	}
	rpm := o.RequestsPerMinute
	if rpm <= 0 {
		rpm = 25
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	lg := o.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &Client{
		base:    base,
		key:     o.APIKey,
		http:    hc,
		limiter: rate.NewLimiter(rate.Limit(float64(rpm)/60), max(rpm/6, 1)),
		log:     lg,
	}
}

var _ Upstream = (*Client)(nil)

const (
	listingsAux = "cmc_rank,date_added,tags,circulating_supply,total_supply,max_supply,platform"
	quotesAux   = "cmc_rank,date_added,tags,circulating_supply,total_supply,max_supply,platform"
	mapAux      = "platform,first_historical_data"
	infoAux     = "urls,platform,description,tags,date_added,date_launched"
)

// ListingsLatest implements Upstream.
func (c *Client) ListingsLatest(ctx context.Context, start, limit int) ([]model.Quote, Meta, error) {
	var r struct {
		Data []cmcAsset `json:"data"`
	}
	meta, err := c.get(ctx, "/v1/cryptocurrency/listings/latest", url.Values{
		"start":   {strconv.Itoa(start)},
		"limit":   {strconv.Itoa(limit)},
		"convert": {"USD"},
		"aux":     {listingsAux},
	}, &r)
	if err != nil {
		return nil, meta, err
	}
	return quotesOf(r.Data), meta, nil
}

// ListingsNew implements Upstream.
func (c *Client) ListingsNew(ctx context.Context, start, limit int) ([]model.Quote, Meta, error) {
	var r struct {
		Data []cmcAsset `json:"data"`
	}
	meta, err := c.get(ctx, "/v1/cryptocurrency/listings/new", url.Values{
		"start":   {strconv.Itoa(start)},
		"limit":   {strconv.Itoa(limit)},
		"convert": {"USD"},
	}, &r)
	if err != nil {
		return nil, meta, err
	}
	return quotesOf(r.Data), meta, nil
}

// QuotesLatest implements Upstream.
func (c *Client) QuotesLatest(ctx context.Context, ids []int64) (map[int64]model.Quote, Meta, error) {
	var r struct {
		Data map[string]cmcAsset `json:"data"`
	}
	meta, err := c.get(ctx, "/v2/cryptocurrency/quotes/latest", url.Values{
		"id":      {joinIDs(ids)},
		"convert": {"USD"},
		"aux":     {quotesAux},
	}, &r)
	if err != nil {
		return nil, meta, err
	}
	out := make(map[int64]model.Quote, len(r.Data))
	for _, a := range r.Data {
		out[a.ID] = a.quote()
	}
	return out, meta, nil
}

// Map implements Upstream.
func (c *Client) Map(ctx context.Context, start, limit int) ([]model.MapEntry, Meta, error) {
	var r struct {
		Data []struct {
			ID                  int64        `json:"id"`
			Name                string       `json:"name"`
			Symbol              string       `json:"symbol"`
			Slug                string       `json:"slug"`
			Rank                int          `json:"rank"`
			IsActive            int          `json:"is_active"`
			FirstHistoricalData flexibleTime `json:"first_historical_data"`
			Platform            *cmcPlatform `json:"platform"`
		} `json:"data"`
	}
	meta, err := c.get(ctx, "/v1/cryptocurrency/map", url.Values{
		"listing_status": {"active"},
		"start":          {strconv.Itoa(start)},
		"limit":          {strconv.Itoa(limit)},
		"sort":           {"cmc_rank"},
		"aux":            {mapAux},
	}, &r)
	if err != nil {
		return nil, meta, err
	}
	out := make([]model.MapEntry, len(r.Data))
	for i, e := range r.Data {
		out[i] = model.MapEntry{
			ID:                  e.ID,
			Symbol:              e.Symbol,
			Name:                e.Name,
			Slug:                e.Slug,
			Rank:                e.Rank,
			IsActive:            e.IsActive == 1,
			FirstHistoricalData: e.FirstHistoricalData.t,
		}
		if e.Platform != nil {
			out[i].Platform = e.Platform.Name
		}
	}
	return out, meta, nil
}

// Info implements Upstream.
func (c *Client) Info(ctx context.Context, ids []int64) (map[int64]model.Info, Meta, error) {
	var r struct {
		Data map[string]struct {
			ID           int64        `json:"id"`
			Name         string       `json:"name"`
			Symbol       string       `json:"symbol"`
			Slug         string       `json:"slug"`
			Category     string       `json:"category"`
			Description  string       `json:"description"`
			Tags         []string     `json:"tags"`
			DateAdded    flexibleTime `json:"date_added"`
			DateLaunched flexibleTime `json:"date_launched"`
			URLs         struct {
				Website []string `json:"website"`
			} `json:"urls"`
			Platform *cmcPlatform `json:"platform"`
		} `json:"data"`
	}
	meta, err := c.get(ctx, "/v2/cryptocurrency/info", url.Values{
		"id":  {joinIDs(ids)},
		"aux": {infoAux},
	}, &r)
	if err != nil {
		return nil, meta, err
	}
	out := make(map[int64]model.Info, len(r.Data))
	for k, d := range r.Data {
		in := model.Info{
			ID:           d.ID,
			Symbol:       d.Symbol,
			Name:         d.Name,
			Slug:         d.Slug,
			Category:     d.Category,
			Description:  d.Description,
			Tags:         d.Tags,
			DateAdded:    d.DateAdded.t,
			DateLaunched: d.DateLaunched.t,
		}
		if len(d.URLs.Website) > 0 {
			in.Website = d.URLs.Website[0]
		}
		if d.Platform != nil {
			in.Platform = d.Platform.Name
		}
		if id, perr := strconv.ParseInt(k, 10, 64); perr == nil && in.ID == 0 {
			in.ID = id
		}
		out[in.ID] = in
	}
	return out, meta, nil
}

// KeyInfo implements Upstream.
func (c *Client) KeyInfo(ctx context.Context) (model.KeyUsage, error) {
	var r struct {
		Data struct {
			Plan struct {
				CreditLimitMonthly      int    `json:"credit_limit_monthly"`
				CreditLimitMonthlyReset string `json:"credit_limit_monthly_reset"`
				RateLimitMinute         int    `json:"rate_limit_minute"`
			} `json:"plan"`
			Usage struct {
				CurrentDay struct {
					CreditsUsed int `json:"credits_used"`
				} `json:"current_day"`
				CurrentMonth struct {
					CreditsUsed int `json:"credits_used"`
					CreditsLeft int `json:"credits_left"`
				} `json:"current_month"`
			} `json:"usage"`
		} `json:"data"`
	}
	_, err := c.get(ctx, "/v1/key/info", nil, &r)
	if err != nil {
		return model.KeyUsage{}, err
	}
	return model.KeyUsage{
		CreditLimitMonthly:      r.Data.Plan.CreditLimitMonthly,
		CreditLimitMonthlyReset: r.Data.Plan.CreditLimitMonthlyReset,
		RateLimitMinute:         r.Data.Plan.RateLimitMinute,
		CreditsUsedToday:        r.Data.Usage.CurrentDay.CreditsUsed,
		CreditsUsedMonth:        r.Data.Usage.CurrentMonth.CreditsUsed,
		CreditsLeftMonth:        r.Data.Usage.CurrentMonth.CreditsLeft,
		FetchedAt:               time.Now(),
	}, nil
}

// get performs one rate-limited GET and decodes the CMC envelope. It
// returns a *APIError on transport failure, HTTP >= 400 or a non-zero
// status.error_code.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) (Meta, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return Meta{}, err
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Meta{}, err
	}
	req.Header.Set("X-CMC_PRO_API_KEY", c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "deflate, gzip")

	resp, err := c.http.Do(req)
	if err != nil {
		return Meta{HTTPStatus: 0}, &APIError{Endpoint: path, Message: err.Error()}
	}
	defer resp.Body.Close()
	body := io.Reader(resp.Body)
	switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
	case "gzip":
		gz, err := gzip.NewReader(body)
		if err != nil {
			return Meta{HTTPStatus: resp.StatusCode}, &APIError{HTTPStatus: resp.StatusCode, Endpoint: path, Message: err.Error()}
		}
		defer gz.Close()
		body = gz
	case "deflate":
		fl := flate.NewReader(body)
		defer fl.Close()
		body = fl
	}
	raw, err := io.ReadAll(io.LimitReader(body, 64<<20))
	if err != nil {
		return Meta{HTTPStatus: resp.StatusCode}, &APIError{HTTPStatus: resp.StatusCode, Endpoint: path, Message: err.Error()}
	}

	var env struct {
		Status *struct {
			Timestamp    time.Time `json:"timestamp"`
			ErrorCode    int       `json:"error_code"`
			ErrorMessage *string   `json:"error_message"`
			Elapsed      int       `json:"elapsed"`
			CreditCount  int       `json:"credit_count"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return Meta{HTTPStatus: resp.StatusCode}, &APIError{HTTPStatus: resp.StatusCode, Endpoint: path, Message: "bad json: " + err.Error()}
	}
	var meta Meta
	meta.HTTPStatus = resp.StatusCode
	code := 0
	msg := ""
	if env.Status != nil {
		meta.Timestamp = env.Status.Timestamp
		meta.Elapsed = time.Duration(env.Status.Elapsed) * time.Millisecond
		meta.CreditCount = env.Status.CreditCount
		code = env.Status.ErrorCode
		if env.Status.ErrorMessage != nil {
			msg = *env.Status.ErrorMessage
		}
	}
	if resp.StatusCode >= 400 || code != 0 {
		ae := &APIError{HTTPStatus: resp.StatusCode, Code: code, Message: msg, Endpoint: path}
		if msg == "" {
			ae.Message = http.StatusText(resp.StatusCode)
		}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
				ae.RetryAfter = time.Duration(secs) * time.Second
			} else if t, err := http.ParseTime(ra); err == nil {
				ae.RetryAfter = max(time.Until(t), time.Second)
			}
		}
		return meta, ae
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return meta, &APIError{HTTPStatus: resp.StatusCode, Endpoint: path, Message: "bad json: " + err.Error()}
		}
	}
	return meta, nil
}

// cmcPlatform is the platform object CMC embeds in assets and map entries.
type cmcPlatform struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Symbol string `json:"symbol"`
	Slug   string `json:"slug"`
}

// flexibleTime decodes CMC timestamps, tolerating null.
type flexibleTime struct{ t time.Time }

func (f *flexibleTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		f.t = time.Time{}
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	f.t = t
	return nil
}

// cmcAsset is the per-asset object CMC returns in listings and quotes.
type cmcAsset struct {
	ID                int64        `json:"id"`
	Name              string       `json:"name"`
	Symbol            string       `json:"symbol"`
	Slug              string       `json:"slug"`
	CMCRank           int          `json:"cmc_rank"`
	CirculatingSupply *float64     `json:"circulating_supply"`
	TotalSupply       *float64     `json:"total_supply"`
	MaxSupply         *float64     `json:"max_supply"`
	DateAdded         flexibleTime `json:"date_added"`
	Tags              []string     `json:"tags"`
	Quote             map[string]struct {
		Price       *float64     `json:"price"`
		Volume24h   *float64     `json:"volume_24h"`
		MarketCap   *float64     `json:"market_cap"`
		Change1h    *float64     `json:"percent_change_1h"`
		Change24h   *float64     `json:"percent_change_24h"`
		Change7d    *float64     `json:"percent_change_7d"`
		LastUpdated flexibleTime `json:"last_updated"`
	} `json:"quote"`
}

func num(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func (a *cmcAsset) quote() model.Quote {
	usd := a.Quote["USD"]
	return model.Quote{
		ID:                a.ID,
		Symbol:            a.Symbol,
		Name:              a.Name,
		Slug:              a.Slug,
		Rank:              a.CMCRank,
		Price:             num(usd.Price),
		MarketCap:         num(usd.MarketCap),
		Volume24h:         num(usd.Volume24h),
		Change1hPct:       num(usd.Change1h),
		Change24hPct:      num(usd.Change24h),
		Change7dPct:       num(usd.Change7d),
		CirculatingSupply: num(a.CirculatingSupply),
		TotalSupply:       num(a.TotalSupply),
		MaxSupply:         a.MaxSupply,
		DateAdded:         a.DateAdded.t,
		Tags:              a.Tags,
		LastUpdated:       usd.LastUpdated.t,
		FetchedAt:         time.Now(),
	}
}

func quotesOf(data []cmcAsset) []model.Quote {
	out := make([]model.Quote, len(data))
	for i := range data {
		out[i] = data[i].quote()
	}
	return out
}

func joinIDs(ids []int64) string {
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	return b.String()
}
