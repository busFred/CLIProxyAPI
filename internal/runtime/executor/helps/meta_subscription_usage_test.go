package helps

import "testing"

func TestParseMetaSubscriptionUsageHeaders(t *testing.T) {
	headers := ParseMetaSubscriptionUsageHeaders([]byte(`{"type":"response.subscription_usage","subscription":{"tier":"1234","window":{"used_percent":12.5,"resets_at":1794425416,"window_duration_mins":300},"weekly":{"used_percent":61,"resets_at":1794787200}}}`))
	want := map[string]string{
		MetaSubscriptionWindowUsedPercentHeader: "12.5",
		MetaSubscriptionWindowResetsAtHeader:    "1794425416",
		MetaSubscriptionWindowMinutesHeader:     "300",
		MetaSubscriptionWeeklyUsedPercentHeader: "61",
		MetaSubscriptionWeeklyResetsAtHeader:    "1794787200",
	}
	for key, value := range want {
		if got := headers.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if len(headers) != len(want) {
		t.Errorf("unexpected extra headers: %#v", headers)
	}
}

func TestParseMetaSubscriptionUsageHeadersPartialAndEmpty(t *testing.T) {
	headers := ParseMetaSubscriptionUsageHeaders([]byte(`{"subscription":{"tier":"x","weekly":{"used_percent":0,"resets_at":1794787200},"window":{"resets_at":"bad"}}}`))
	if headers.Get(MetaSubscriptionWeeklyUsedPercentHeader) != "0" || headers.Get(MetaSubscriptionWindowResetsAtHeader) != "" {
		t.Fatalf("unexpected partial headers: %#v", headers)
	}
	for _, payload := range []string{``, `{}`, `{"subscription":null}`, `{"subscription":{"tier":"x"}}`, `not json`} {
		if got := ParseMetaSubscriptionUsageHeaders([]byte(payload)); got != nil {
			t.Errorf("ParseMetaSubscriptionUsageHeaders(%q) = %#v, want nil", payload, got)
		}
	}
}
