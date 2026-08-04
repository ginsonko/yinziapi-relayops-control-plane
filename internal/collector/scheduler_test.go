package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/connectors"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

func TestSchedulerLetsIdleWorkerAdvancePastSlowEarlierSupplier(t *testing.T) {
	t.Parallel()
	releaseSlow := make(chan struct{})
	slowStarted := make(chan struct{}, 1)
	thirdStarted := make(chan struct{}, 1)
	newUpstream := func(block bool, started chan<- struct{}) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/status":
				_, _ = fmt.Fprint(w, `{"success":true,"data":{"quota_per_unit":500000,"currency":"USD"}}`)
			case "/api/pricing":
				if started != nil {
					select {
					case started <- struct{}{}:
					default:
					}
				}
				if block {
					<-releaseSlow
				}
				_, _ = fmt.Fprint(w, pricingPayload("0.05"))
			default:
				http.NotFound(w, r)
			}
		}))
	}
	slow := newUpstream(true, slowStarted)
	second := newUpstream(false, nil)
	third := newUpstream(false, thirdStarted)
	defer slow.Close()
	defer second.Close()
	defer third.Close()

	st := openCollectorStore(t, "scheduler.db")
	service := NewService(st, connectors.NewFactory(http.DefaultClient, nil))
	base := time.Now().UTC().Add(-time.Minute)
	for index, upstream := range []*httptest.Server{slow, second, third} {
		supplier := domain.Supplier{Name: fmt.Sprintf("supplier-%d", index), BaseURL: upstream.URL,
			Adapter: "newapi", Enabled: true, PollIntervalSeconds: 300}
		if err := st.CreateSupplier(context.Background(), &supplier); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkSupplierResult(context.Background(), supplier.ID, "healthy", "", base,
			base.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	scheduler := NewScheduler(st, service, time.Hour, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		scheduler.Run(ctx)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("scheduler workers did not stop after cancellation")
		}
	}()
	select {
	case <-slowStarted:
	case <-time.After(3 * time.Second):
		close(releaseSlow)
		t.Fatal("slow supplier did not start")
	}
	select {
	case <-thirdStarted:
		// The second worker completed supplier two and immediately advanced.
	case <-time.After(3 * time.Second):
		close(releaseSlow)
		t.Fatal("later due supplier was blocked behind the slow batch member")
	}
	close(releaseSlow)
}
