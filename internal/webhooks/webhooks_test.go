package webhooks

import (
	"strings"
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	secret, body := "whsec_test", []byte(`{"id":"evt_1"}`)
	at := time.Unix(1_800_000_000, 0)
	sig := Sign(secret, at, body)
	if !strings.HasPrefix(sig, "t=1800000000,v1=") || len(sig) != len("t=1800000000,v1=")+64 {
		t.Fatalf("signature format: %s", sig)
	}
	if err := Verify(secret, sig, body, at.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	cases := map[string]struct {
		secret, header string
		body           []byte
		now            time.Time
	}{
		"tampered body":     {secret, sig, []byte(`{"id":"evt_2"}`), at},
		"wrong secret":      {"whsec_other", sig, body, at},
		"replayed later":    {secret, sig, body, at.Add(10 * time.Minute)},
		"from the future":   {secret, sig, body, at.Add(-10 * time.Minute)},
		"missing timestamp": {secret, strings.Split(sig, ",")[1], body, at},
		"missing signature": {secret, "t=1800000000", body, at},
		"garbage":           {secret, "nonsense", body, at},
		"timestamp swapped": {secret, strings.Replace(sig, "t=1800000000", "t=1800000001", 1), body, at},
	}
	for name, c := range cases {
		if err := Verify(c.secret, c.header, c.body, c.now, 5*time.Minute); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Several v1 values: any match is enough (overlapping secrets).
	both := sig + ",v1=" + strings.Repeat("0", 64)
	if err := Verify(secret, both, body, at, time.Minute); err != nil {
		t.Fatalf("extra v1 rejected: %v", err)
	}
}

func TestBackoffSchedule(t *testing.T) {
	var total time.Duration
	for n := 1; n < MaxAttempts; n++ {
		d := Backoff(n)
		if n > 1 && d < Backoff(n-1) {
			t.Fatalf("backoff shrinks at attempt %d", n)
		}
		total += d
	}
	if MaxAttempts != 10 || total < 24*time.Hour || total > 30*time.Hour {
		t.Fatalf("max attempts %d, total retry window %v; want 10 attempts over about a day", MaxAttempts, total)
	}
	if Backoff(1) != 10*time.Second || Backoff(100) != Backoff(MaxAttempts-1) {
		t.Fatalf("Backoff(1)=%v Backoff(100)=%v", Backoff(1), Backoff(100))
	}
}

func TestValidateEndpoint(t *testing.T) {
	good := Endpoint{URL: " https://hooks.example.com/clockwork ", Events: []string{"execution.failed", "execution.failed", "execution.started"}}
	if err := validate(&good); err != nil {
		t.Fatal(err)
	}
	if good.URL != "https://hooks.example.com/clockwork" || len(good.Events) != 2 || good.Events[0] != "execution.failed" {
		t.Fatalf("not normalised: %+v", good)
	}
	for field, e := range map[string]Endpoint{
		"url":         {URL: "ftp://example.com"},
		"url ":        {URL: "https://user:pw@example.com"},
		"url  ":       {URL: ""},
		"events":      {URL: "https://x.dev", Events: []string{"task.created"}},
		"description": {URL: "https://x.dev", Description: strings.Repeat("x", 2001)},
	} {
		err := validate(&e)
		ve, ok := err.(*ValidationError)
		if !ok || ve.Fields[strings.TrimSpace(field)] == "" {
			t.Errorf("%s: %v", field, err)
		}
	}
	if s := newSecret(); !strings.HasPrefix(s, "whsec_") || len(s) < 50 || s == newSecret() {
		t.Fatalf("secret: %q", s)
	}
}
