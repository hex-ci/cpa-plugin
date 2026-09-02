package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchUserResourceConcurrencyLimit(t *testing.T) {
	for _, calls := range []int{10, 20, 50} {
		t.Run(fmt.Sprintf("%d calls", calls), func(t *testing.T) {
			entered := make(chan struct{}, calls)
			release := make(chan struct{})

			var inFlight, maxInFlight int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				current := atomic.AddInt32(&inFlight, 1)
				for {
					max := atomic.LoadInt32(&maxInFlight)
					if current <= max || atomic.CompareAndSwapInt32(&maxInFlight, max, current) {
						break
					}
				}
				defer atomic.AddInt32(&inFlight, -1)

				entered <- struct{}{}
				<-release
				_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"TotalCount":0,"Accounts":[]}}}}`))
			}))
			defer func() {
				close(release)
				srv.Close()
			}()
			restoreBillingBase := setBillingBase(srv.URL)
			defer restoreBillingBase()

			errs := make(chan error, calls)
			var wg sync.WaitGroup
			for range calls {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := fetchUserResource(&storedAuth{Auth: storedTokens{AccessToken: "test-token"}})
					errs <- err
				}()
			}

			for range 4 {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for resource fetch")
				}
			}
			select {
			case <-entered:
				t.Fatal("more than four resource fetches reached billing before a slot released")
			case <-time.After(500 * time.Millisecond):
			}
			for i := 0; i < calls; i++ {
				if i >= 4 {
					select {
					case <-entered:
					case <-time.After(5 * time.Second):
						t.Fatal("timed out waiting for resource fetch")
					}
				}
				release <- struct{}{}
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("fetchUserResource: %v", err)
				}
			}
			if got := atomic.LoadInt32(&maxInFlight); got > 4 {
				t.Fatalf("max in-flight requests = %d, want <= 4", got)
			}
		})
	}
}
