// Command webhook-receiver is an example endpoint for Clockwork webhooks. It
// uses only the standard library so the verification code can be copied into
// any Go service.
//
// It shows what a receiver must do:
//
//  1. Verify the Clockwork-Signature header against the raw body, and reject
//     old timestamps (replays).
//  2. Answer 2xx quickly. Anything else, or no answer within 10 seconds, is
//     retried with growing delays for about a day.
//  3. Deduplicate on Clockwork-Delivery: delivery is at-least-once, and a
//     retried delivery keeps its ID.
//
// Usage:
//
//	WEBHOOK_SECRET=whsec_... go run ./examples/webhook-receiver   # listens on :9000
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// verify checks "t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>".
func verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return errors.New("malformed signature header")
	}
	if age := now.Sub(time.Unix(sec, 0)); age > tolerance || age < -tolerance {
		return errors.New("timestamp outside tolerance")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return nil
		}
	}
	return errors.New("signature mismatch")
}

type event struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Execution struct {
			ID       string `json:"id"`
			TaskName string `json:"task_name"`
			Status   string `json:"status"`
			Attempt  int    `json:"attempt"`
		} `json:"execution"`
	} `json:"data"`
}

func main() {
	secret := os.Getenv("WEBHOOK_SECRET")
	if secret == "" {
		log.Fatal("set WEBHOOK_SECRET to the endpoint's signing secret (whsec_...)")
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":9000"
	}

	var (
		mu   sync.Mutex
		seen = map[string]bool{} // a real service would use a table with a unique key
	)
	http.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		if err := verify(secret, r.Header.Get("Clockwork-Signature"), body, time.Now(), 5*time.Minute); err != nil {
			log.Printf("rejected: %v", err)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		delivery := r.Header.Get("Clockwork-Delivery")
		mu.Lock()
		dup := seen[delivery]
		seen[delivery] = true
		mu.Unlock()
		if dup {
			log.Printf("duplicate delivery %s ignored", delivery)
			w.WriteHeader(http.StatusOK) // still 2xx, so Clockwork stops retrying
			return
		}

		var ev event
		_ = json.Unmarshal(body, &ev)
		x := ev.Data.Execution
		if x.ID != "" {
			log.Printf("%s: task %q execution %s attempt %d is %s", ev.Type, x.TaskName, x.ID, x.Attempt, x.Status)
		} else {
			log.Printf("%s (%s)", ev.Type, ev.ID)
		}
		// Do slow work asynchronously; answer within a few seconds.
		fmt.Fprintln(w, "ok")
	})
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
