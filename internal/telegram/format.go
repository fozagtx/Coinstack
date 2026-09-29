package telegram

import (
	"fmt"
	"html"
)

// esc escapes text for parse_mode=HTML.
func esc(s string) string { return html.EscapeString(s) }

// money renders a USD amount compactly ($12.4M, $345k).
func money(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("$%.2fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("$%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.0fk", v/1e3)
	default:
		return fmt.Sprintf("$%.0f", v)
	}
}

// price renders a price with enough precision for cheap assets.
func price(v float64) string {
	switch {
	case v >= 1:
		return fmt.Sprintf("%.2f", v)
	case v >= 0.0001:
		return fmt.Sprintf("%.6f", v)
	default:
		return fmt.Sprintf("%.8g", v)
	}
}
