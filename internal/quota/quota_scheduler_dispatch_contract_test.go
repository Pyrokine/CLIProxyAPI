package quota

import (
	"testing"
	"testing/synctest"
	"time"
)

func TestQuotaDispatcherRechecksDisabledBindingAfterWake(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			calls := make(chan string, 4)
			cfg := DefaultConfig()
			cfg.Enabled = true
			s := NewScheduler(
				cfg, func(entry *Entry) ([]byte, error) {
					calls <- entry.FileName
					return []byte(`{"rate_limit":{"allowed":true}}`), nil
				},
			)
			s.Register("first.json", TypeCodex, "first")
			s.Register("later.json", TypeCodex, "later")
			s.Start()
			defer s.Stop()
			synctest.Wait()
			if got := <-calls; got != "first.json" {
				t.Fatalf("initial dispatch=%s", got)
			}
			time.Sleep(time.Second)
			s.SetBindingDisabled("later.json", TypeCodex, "later", true)
			synctest.Wait()
			time.Sleep(3 * time.Second)
			synctest.Wait()
			select {
			case got := <-calls:
				t.Fatalf("disabled binding made an automatic request: %s", got)
			default:
			}
			s.SetBindingDisabled("later.json", TypeCodex, "later", false)
			synctest.Wait()
			select {
			case got := <-calls:
				if got != "later.json" {
					t.Fatalf("re-enabled dispatch=%s", got)
				}
			default:
				t.Fatal("re-enabled due binding was not dispatched")
			}
		},
	)
}
