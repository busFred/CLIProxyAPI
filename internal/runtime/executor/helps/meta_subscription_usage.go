package helps

import (
	"net/http"
	"strconv"

	"github.com/MoeForge/moego/ojson"
)

// Synthetic response headers carrying Meta Muse subscription usage. Meta sends
// this data only as a response.subscription_usage SSE event, which the meta
// executor filters from client output; the headers expose it to usage records
// (and so to usage plugins) without reaching the client.
const (
	MetaSubscriptionWindowUsedPercentHeader = "X-Muse-Subscription-Window-Used-Percent"
	MetaSubscriptionWindowResetsAtHeader    = "X-Muse-Subscription-Window-Resets-At"
	MetaSubscriptionWindowMinutesHeader     = "X-Muse-Subscription-Window-Minutes"
	MetaSubscriptionWeeklyUsedPercentHeader = "X-Muse-Subscription-Weekly-Used-Percent"
	MetaSubscriptionWeeklyResetsAtHeader    = "X-Muse-Subscription-Weekly-Resets-At"
)

// ParseMetaSubscriptionUsageHeaders converts the "subscription" object of a
// Meta response.subscription_usage event (or a JSON body carrying one at its
// root) into synthetic X-Muse-Subscription-* headers. It returns nil when the
// payload carries no usable window.
func ParseMetaSubscriptionUsageHeaders(eventData []byte) http.Header {
	if len(eventData) == 0 {
		return nil
	}
	subscription := ojson.GetBytesNoCopy(eventData, "subscription")
	if !subscription.IsObject() {
		return nil
	}
	headers := make(http.Header)
	window := subscription.Get("window")
	if setMetaSubscriptionNumber(headers, MetaSubscriptionWindowUsedPercentHeader, window.Get("used_percent")) {
		setMetaSubscriptionNumber(headers, MetaSubscriptionWindowResetsAtHeader, window.Get("resets_at"))
		setMetaSubscriptionNumber(headers, MetaSubscriptionWindowMinutesHeader, window.Get("window_duration_mins"))
	}
	weekly := subscription.Get("weekly")
	if setMetaSubscriptionNumber(headers, MetaSubscriptionWeeklyUsedPercentHeader, weekly.Get("used_percent")) {
		setMetaSubscriptionNumber(headers, MetaSubscriptionWeeklyResetsAtHeader, weekly.Get("resets_at"))
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func setMetaSubscriptionNumber(headers http.Header, key string, value ojson.Result) bool {
	if value.Type != ojson.Number {
		return false
	}
	headers.Set(key, strconv.FormatFloat(value.Float(), 'f', -1, 64))
	return true
}
