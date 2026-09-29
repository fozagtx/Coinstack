package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// openapi.go builds the OpenAPI 3.0.3 document at startup from the
// endpointParams table, so paths, parameters and validation stay in sync
// with the handlers.

// paramDoc provides the human description and example for one parameter.
type paramDoc struct {
	desc    string
	example string
}

// paramDocs holds the per-endpoint parameter documentation, keyed by
// path then parameter name.
var paramDocs = map[string]map[string]paramDoc{
	pathGems: {
		"max_market_cap":     {"Only assets below this market cap are scored.", "50000000"},
		"min_market_cap":     {"Only assets above this market cap are scored.", "1000000"},
		"min_volume":         {"Minimum 24 h volume in USD.", "100000"},
		"listed_within_days": {"Only assets first listed within this many days; 0 disables.", "30"},
		"sector":             {"Only assets carrying this CMC tag, e.g. ai-big-data.", "ai-big-data"},
		"include_pumped":     {"Keep gems flagged already_pumped (24h > 100% or 7d > 300%).", "false"},
		"limit":              {"Maximum number of gems, 1-50.", "10"},
	},
	pathScreen: {
		"min_market_cap":      {"Minimum market cap in USD.", "1000000"},
		"max_market_cap":      {"Maximum market cap in USD.", "50000000"},
		"min_volume":          {"Minimum 24 h volume in USD.", "100000"},
		"max_volume":          {"Maximum 24 h volume in USD.", "5000000"},
		"min_turnover":        {"Minimum volume_24h / market_cap.", "0.1"},
		"min_change_1h_pct":   {"Minimum 1 h change in percent.", "-5"},
		"max_change_1h_pct":   {"Maximum 1 h change in percent.", "20"},
		"min_change_24h_pct":  {"Minimum 24 h change in percent.", "10"},
		"max_change_24h_pct":  {"Maximum 24 h change in percent.", "80"},
		"min_change_7d_pct":   {"Minimum 7 d change in percent.", "-20"},
		"max_change_7d_pct":   {"Maximum 7 d change in percent.", "150"},
		"tag":                 {"Comma-separated CMC tags; matches assets carrying any of them (max 10).", "depin,gaming"},
		"listed_within_days":  {"Only assets first listed within this many days; 0 disables.", "14"},
		"exclude_stablecoins": {"Drop stablecoins and wrapped tokens (default true).", "true"},
		"sort":                {"Sort key.", "change_24h_pct"},
		"order":               {"asc or desc; default desc, except sort=rank which defaults to asc.", "desc"},
		"limit":               {"Maximum number of assets, 1-50.", "10"},
	},
	pathClimbers: {
		"window":         {"History window to compare rank against: 24h or 7d.", "24h"},
		"direction":      {"up lists biggest rank climbs, down the biggest drops.", "up"},
		"min_volume":     {"Minimum 24 h volume in USD.", "100000"},
		"max_market_cap": {"Maximum market cap in USD; 0 disables.", "50000000"},
		"limit":          {"Maximum number of climbers, 1-50.", "10"},
	},
	pathNewListings: {
		"days":       {"Only assets first listed within this many days, 1-30.", "7"},
		"min_volume": {"Minimum 24 h volume in USD.", "0"},
		"limit":      {"Maximum number of listings, 1-50.", "10"},
	},
	pathSectors: {
		"sort":        {"Sort key for the sector list.", "heat"},
		"min_members": {"Minimum tag membership for a sector to be tracked, 2-200.", "5"},
		"sector":      {"Return detail for this one tag instead of the list.", "depin"},
		"limit":       {"Maximum sectors (or members, in detail view), 1-50.", "10"},
	},
	pathAsset: {
		"asset": {"Symbol, name, slug or CMC id of the asset.", "SOL"},
	},
	pathResolve: {
		"query": {"Text to resolve: symbol, name, slug or CMC id.", "uni"},
		"limit": {"Maximum candidates, 1-20.", "5"},
	},
}

// endpointDocs holds each endpoint's one-line summary and data shape note.
var endpointDocs = map[string]struct {
	summary string
	data    string // jsonSchema ref name for the data payload
}{
	pathGems:        {"Ranked altcoin candidates with a transparent composite score.", "#/components/schemas/GemItem"},
	pathScreen:      {"Filter the cached universe by cap, volume, turnover, changes, tags and age.", "#/components/schemas/AssetItem"},
	pathClimbers:    {"Assets that climbed or dropped the most CMC ranks over a window.", "#/components/schemas/ClimberItem"},
	pathNewListings: {"Assets first listed on CoinMarketCap within the last days.", "#/components/schemas/NewListingItem"},
	pathSectors:     {"Sector (tag) aggregates with heat, or one sector's member list.", "#/components/schemas/Sector"},
	pathAsset:       {"Full detail for one asset, including its signal breakdown.", "#/components/schemas/AssetDetail"},
	pathResolve:     {"Resolve a symbol, name, slug or id to candidate assets.", "#/components/schemas/ResolveResult"},
	pathHealth:      {"Service health: poller status, cache sizes, credits, request metrics.", "#/components/schemas/Health"},
}

func schemaFor(spec paramSpec) map[string]any {
	switch spec.kind {
	case kindInt:
		s := map[string]any{"type": "integer"}
		if spec.max > 0 {
			s["minimum"], s["maximum"] = spec.min, spec.max
		}
		return s
	case kindBool:
		return map[string]any{"type": "boolean", "default": spec.def == "true"}
	case kindEnum:
		return map[string]any{"type": "string", "enum": spec.enum}
	case kindNumber:
		return map[string]any{"type": "number"}
	default:
		return map[string]any{"type": "string"}
	}
}

func paramObject(spec paramSpec, doc paramDoc) map[string]any {
	p := map[string]any{
		"name":        spec.name,
		"in":          "query",
		"required":    spec.required,
		"schema":      schemaFor(spec),
		"description": doc.desc,
	}
	if doc.example != "" {
		p["example"] = doc.example
	}
	if spec.def != "" {
		p["schema"].(map[string]any)["default"] = spec.def
	}
	return p
}

func envelopeSchema(dataRef string, dataArray bool) map[string]any {
	data := map[string]any{}
	if dataRef != "" {
		if dataArray {
			data = map[string]any{"type": "array", "items": map[string]any{"$ref": dataRef}}
		} else {
			data = map[string]any{"$ref": dataRef}
		}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"as_of":         map[string]any{"type": "string", "format": "date-time"},
			"age_seconds":   map[string]any{"type": "integer"},
			"source":        map[string]any{"type": "string", "enum": []string{"coinmarketcap"}},
			"note":          map[string]any{"type": "string"},
			"history_hours": map[string]any{"type": "integer"},
			"data":          data,
			"warnings":      map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Warning"}},
		},
	}
}

func errorSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"error": map[string]any{"$ref": "#/components/schemas/Error"},
		},
	}
}

func itemFields() map[string]any {
	return map[string]any{
		"id":             map[string]any{"type": "integer"},
		"symbol":         map[string]any{"type": "string"},
		"name":           map[string]any{"type": "string"},
		"rank":           map[string]any{"type": "integer"},
		"price":          map[string]any{"type": "number"},
		"market_cap":     map[string]any{"type": "number"},
		"volume_24h":     map[string]any{"type": "number"},
		"turnover":       map[string]any{"type": "number"},
		"change_1h_pct":  map[string]any{"type": "number"},
		"change_24h_pct": map[string]any{"type": "number"},
		"change_7d_pct":  map[string]any{"type": "number"},
		"date_added":     map[string]any{"type": "string", "format": "date-time", "nullable": true},
		"tags":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"last_updated":   map[string]any{"type": "string", "format": "date-time"},
	}
}

func allOf(extra map[string]any) map[string]any {
	return map[string]any{
		"allOf": []any{
			map[string]any{"$ref": "#/components/schemas/AssetItem"},
			map[string]any{"type": "object", "properties": extra},
		},
	}
}

// buildOpenAPI renders the OpenAPI document and its ETag.
func buildOpenAPI(version string) ([]byte, string) {
	num := map[string]any{"type": "number"}
	str := map[string]any{"type": "string"}
	strArr := map[string]any{"type": "array", "items": str}
	component := map[string]any{
		"type": "object", "properties": map[string]any{
			"score":  num,
			"detail": map[string]any{"type": "object", "additionalProperties": num},
		},
	}
	schemas := map[string]any{
		"AssetItem": map[string]any{"type": "object", "properties": itemFields()},
		"AssetDetail": allOf(map[string]any{
			"slug":               str,
			"circulating_supply": num,
			"total_supply":       num,
			"max_supply":         map[string]any{"type": "number", "nullable": true},
			"category":           str,
			"platform":           str,
			"website":            str,
			"score":              num,
			"confidence":         map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
			"eligible_for_gems":  map[string]any{"type": "boolean"},
			"signals":            map[string]any{"$ref": "#/components/schemas/Signals"},
			"risk_flags":         strArr,
			"why":                strArr,
			"hot_sectors":        strArr,
		}),
		"GemItem": allOf(map[string]any{
			"score":       num,
			"confidence":  map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
			"signals":     map[string]any{"$ref": "#/components/schemas/Signals"},
			"risk_flags":  strArr,
			"why":         strArr,
			"hot_sectors": strArr,
		}),
		"ClimberItem": allOf(map[string]any{
			"rank_then":       map[string]any{"type": "integer"},
			"rank_change":     map[string]any{"type": "integer"},
			"rank_change_pct": num,
		}),
		"NewListingItem": allOf(map[string]any{
			"days_listed":                  num,
			"rank_change_since_first_seen": map[string]any{"type": "integer", "nullable": true},
		}),
		"Sector": map[string]any{"type": "object", "properties": map[string]any{
			"tag":                   str,
			"members":               map[string]any{"type": "integer"},
			"median_change_24h_pct": num,
			"median_change_7d_pct":  num,
			"total_volume_24h":      num,
			"total_market_cap":      num,
			"heat":                  num,
			"leaders": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "properties": map[string]any{
					"id": map[string]any{"type": "integer"}, "symbol": str, "name": str, "change_24h_pct": num,
				},
			}},
		}},
		"Signals": map[string]any{"type": "object", "properties": map[string]any{
			"turnover": component, "new_listing": component, "rank_climb": component, "sector_heat": component,
		}},
		"ResolveResult": map[string]any{"type": "object", "properties": map[string]any{
			"candidates":  map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Candidate"}},
			"resolved_id": map[string]any{"type": "integer", "nullable": true},
		}},
		"Candidate": map[string]any{"type": "object", "properties": map[string]any{
			"id": map[string]any{"type": "integer"}, "symbol": str, "name": str, "slug": str,
			"rank": map[string]any{"type": "integer"}, "platform": str, "match": str,
		}},
		"Health": map[string]any{"type": "object"},
		"Warning": map[string]any{"type": "object", "properties": map[string]any{
			"code": str, "message": str, "query": str,
			"chosen_id":   map[string]any{"type": "integer"},
			"age_seconds": map[string]any{"type": "integer"},
			"candidates":  map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Candidate"}},
		}},
		"Error": map[string]any{"type": "object", "properties": map[string]any{
			"code": str, "message": str, "next_step": str, "param": str, "query": str,
			"allowed_values":      strArr,
			"candidates":          map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Candidate"}},
			"retry_after_seconds": map[string]any{"type": "integer"},
		}},
	}

	paths := map[string]any{}
	for path, specs := range endpointParams {
		d := endpointDocs[path]
		params := make([]any, 0, len(specs))
		for _, spec := range specs {
			params = append(params, paramObject(spec, paramDocs[path][spec.name]))
		}
		dataArray := true
		switch path {
		case pathAsset, pathResolve, pathHealth:
			dataArray = false
		}
		paths[path] = map[string]any{
			"get": map[string]any{
				"summary":    d.summary,
				"parameters": params,
				"responses": map[string]any{
					"200": map[string]any{
						"description": "Success envelope.",
						"content":     jsonContent(envelopeSchema(d.data, dataArray)),
					},
					"400": errResp("Invalid or unknown parameter."),
					"401": errResp("Missing or invalid API key."),
					"404": errResp("Asset or sector not found."),
					"503": errResp("Upstream CoinMarketCap unavailable."),
				},
				"security": []any{map[string]any{"bearerAuth": []any{}}},
			},
		}
	}
	paths[pathDocs] = map[string]any{
		"get": map[string]any{
			"summary":   "Human-readable documentation page.",
			"security":  []any{},
			"responses": map[string]any{"200": map[string]any{"description": "HTML docs page."}},
		},
	}
	paths[pathOpenAPI] = map[string]any{
		"get": map[string]any{
			"summary":   "This OpenAPI document.",
			"security":  []any{},
			"responses": map[string]any{"200": map[string]any{"description": "OpenAPI 3.0.3 JSON.", "content": jsonContent(map[string]any{"type": "object"})}},
		},
	}
	for _, p := range []string{pathOpenAPI, pathHealth} {
		if get, ok := paths[p].(map[string]any)["get"].(map[string]any); ok {
			get["security"] = []any{}
		}
	}

	doc := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "CoinStack",
			"version":     version,
			"description": "Altcoin-discovery API for AI agents: early, high-upside altcoins with a transparent score. Market data for information only, not financial advice.",
		},
		"paths": paths,
		"components": map[string]any{
			"schemas": schemas,
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer", "description": "API key; X-API-Key header is also accepted."},
			},
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(fmt.Sprintf("api: openapi marshal: %v", err))
	}
	sum := sha256.Sum256(raw)
	return raw, `"` + hex.EncodeToString(sum[:16]) + `"`
}

func jsonContent(schema map[string]any) map[string]any {
	return map[string]any{"application/json": map[string]any{"schema": schema}}
}

func errResp(desc string) map[string]any {
	return map[string]any{"description": desc, "content": jsonContent(errorSchema())}
}
