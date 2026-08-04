package targets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type controlledOptionSite struct {
	mu                 sync.Mutex
	ratio              map[string]json.Number
	directContract     bool
	putReject          bool
	pricingFailure     bool
	pricingVisible     map[string]bool
	putCount           int
	statusDataFalse    bool
	channel            newAPIChannel
	routingRequestBody map[string]any
}

func (site *controlledOptionSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer management-token" || r.Header.Get("New-Api-User") != "17" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	site.mu.Lock()
	defer site.mu.Unlock()
	switch {
	case r.URL.Path == "/api/option/" && r.Method == http.MethodGet:
		encoded, _ := json.Marshal(site.ratio)
		if site.directContract {
			fmt.Fprintf(w, `{"success":true,"data":{"GroupRatio":%s}}`, encoded)
		} else {
			fmt.Fprintf(w, `{"success":true,"data":[{"key":"GroupRatio","value":%q}]}`, string(encoded))
		}
	case r.URL.Path == "/api/option/" && r.Method == http.MethodPut:
		site.putCount++
		if site.putReject {
			fmt.Fprint(w, `{"success":false,"message":"locked by upstream"}`)
			return
		}
		var input struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		var next map[string]json.Number
		if input.Key != "GroupRatio" || json.Unmarshal([]byte(input.Value), &next) != nil {
			http.Error(w, "bad option", http.StatusBadRequest)
			return
		}
		site.ratio = next
		fmt.Fprint(w, `{"success":true}`)
	case r.URL.Path == "/api/pricing":
		if site.pricingFailure {
			http.Error(w, "pricing unavailable", http.StatusServiceUnavailable)
			return
		}
		effective := site.ratio
		if site.pricingVisible != nil {
			effective = make(map[string]json.Number)
			for group, value := range site.ratio {
				if site.pricingVisible[group] {
					effective[group] = value
				}
			}
		}
		encoded, _ := json.Marshal(effective)
		fmt.Fprintf(w, `{"success":true,"data":[],"group_ratio":%s}`, encoded)
	case r.URL.Path == "/api/channel/" && r.Method == http.MethodGet:
		encoded, _ := json.Marshal(site.channel)
		fmt.Fprintf(w, `{"success":true,"data":{"items":[%s],"total":1}}`, encoded)
	case r.URL.Path == "/api/channel/" && r.Method == http.MethodPut:
		_ = json.NewDecoder(r.Body).Decode(&site.routingRequestBody)
		if value, ok := site.routingRequestBody["group"].(string); ok {
			site.channel.Group = value
		}
		fmt.Fprint(w, `{"success":true,"data":{}}`)
	case r.URL.Path == "/api/channel/5" && r.Method == http.MethodGet:
		encoded, _ := json.Marshal(site.channel)
		fmt.Fprintf(w, `{"success":true,"data":%s}`, encoded)
	case r.URL.Path == "/api/channel/5/status" && r.Method == http.MethodPost:
		if site.statusDataFalse {
			fmt.Fprint(w, `{"success":true,"data":false}`)
			return
		}
		var input struct {
			Status int `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		site.channel.Status = input.Status
		fmt.Fprint(w, `{"success":true,"data":true}`)
	default:
		http.NotFound(w, r)
	}
}

func newControlledOptionSite() *controlledOptionSite {
	baseURL := "https://upstream.example"
	priority := int64(10)
	weight := uint(20)
	autoBan := 1
	modelMapping := `{"alias":"gpt-test"}`
	remark := "keep-me"
	return &controlledOptionSite{
		ratio: map[string]json.Number{"default": "1", "economy": "0.05"},
		channel: newAPIChannel{ID: 5, Name: "upstream", Type: 1, Status: channelStatusEnabled, BaseURL: &baseURL,
			Models: "gpt-test,gpt-other", Group: "economy", Priority: &priority, Weight: &weight, AutoBan: &autoBan,
			ModelMapping: &modelMapping, Remark: &remark},
	}
}

func TestValidatePricingOptionValuesRejectsUnsafeMaps(t *testing.T) {
	t.Parallel()
	for name, values := range map[string]map[string]string{
		"empty map":       {},
		"empty key":       {"": "1"},
		"negative":        {"g": "-1"},
		"not numeric":     {"g": "NaN"},
		"oversized value": {"g": strings.Repeat("1", 129)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidatePricingOptionValues(values); err == nil {
				t.Fatalf("values %#v unexpectedly accepted", values)
			}
		})
	}
}

func TestTargetCapabilitiesSeparateAuthorizationAndUnknownWriteContract(t *testing.T) {
	t.Parallel()
	t.Run("401 and 403", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/option/" {
				http.Error(w, "expired", http.StatusUnauthorized)
				return
			}
			http.Error(w, "forbidden", http.StatusForbidden)
		}))
		defer server.Close()
		adapter, target := newTargetAdapter(t, server)
		matrix := adapter.Capabilities(context.Background(), target)
		if matrix.PricingOptions["GroupRatio"].Read.Status != "unauthorized" || matrix.Channels.Read.Status != "forbidden" {
			t.Fatalf("capability matrix = %#v", matrix)
		}
	})

	t.Run("unknown option contract remains read-only", func(t *testing.T) {
		site := newControlledOptionSite()
		site.directContract = true
		server := httptest.NewServer(site)
		defer server.Close()
		adapter, target := newTargetAdapter(t, server)
		matrix := adapter.Capabilities(context.Background(), target)
		group := matrix.PricingOptions["GroupRatio"]
		if !group.Read.Available || group.Write.Available || group.Write.Status != "unknown_contract" {
			t.Fatalf("GroupRatio capability = %#v", group)
		}
	})
}

func TestControlledPricingOptionHandlesRejectionTimeoutAndCompensation(t *testing.T) {
	t.Parallel()
	t.Run("200 success false is a definitive rejection", func(t *testing.T) {
		site := newControlledOptionSite()
		site.putReject = true
		server := httptest.NewServer(site)
		defer server.Close()
		adapter, target := newTargetAdapter(t, server)
		result, err := adapter.ApplyPricingOptionControlled(context.Background(), target, "GroupRatio",
			HashDecimalMap(map[string]string{"default": "1", "economy": "0.05"}),
			map[string]string{"default": "1", "economy": "0.08"}, func() error { return nil })
		if err == nil || result.ResultState != "rejected_before_write" {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("response timeout after write is recovered by readback", func(t *testing.T) {
		site := newControlledOptionSite()
		server := httptest.NewServer(site)
		defer server.Close()
		adapter, target := newTargetAdapter(t, server)
		baseTransport := server.Client().Transport
		adapter.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response, err := baseTransport.RoundTrip(request)
			if request.Method == http.MethodPut && request.URL.Path == "/api/option/" && err == nil {
				_ = response.Body.Close()
				return nil, context.DeadlineExceeded
			}
			return response, err
		})}
		result, err := adapter.ApplyPricingOptionControlled(context.Background(), target, "GroupRatio",
			HashDecimalMap(map[string]string{"default": "1", "economy": "0.05"}),
			map[string]string{"default": "1", "economy": "0.08"}, func() error { return nil })
		if err != nil || result.ResultState != "verified_success" || result.ReadbackHash != result.DesiredHash {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	})

	t.Run("effective pricing failure triggers verified compensation", func(t *testing.T) {
		site := newControlledOptionSite()
		site.pricingFailure = true
		server := httptest.NewServer(site)
		defer server.Close()
		adapter, target := newTargetAdapter(t, server)
		original := map[string]string{"default": "1", "economy": "0.05"}
		result, err := adapter.ApplyPricingOptionControlled(context.Background(), target, "GroupRatio",
			HashDecimalMap(original), map[string]string{"default": "1", "economy": "0.08"}, func() error { return nil })
		if err == nil || result.ResultState != "rolled_back_verified" {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		site.mu.Lock()
		defer site.mu.Unlock()
		if site.ratio["economy"].String() != "0.05" || site.putCount != 2 {
			t.Fatalf("compensated state ratio=%v puts=%d", site.ratio, site.putCount)
		}
	})
}

func TestControlledPricingOptionAcceptsVerifiedEffectiveSubset(t *testing.T) {
	t.Parallel()
	site := newControlledOptionSite()
	site.ratio["inactive"] = json.Number("0.2")
	site.pricingVisible = map[string]bool{"default": true, "economy": true}
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	result, err := adapter.ApplyPricingOptionControlled(context.Background(), target, "GroupRatio",
		HashDecimalMap(map[string]string{"default": "1", "economy": "0.05", "inactive": "0.2"}),
		map[string]string{"default": "1", "economy": "0.08", "inactive": "0.2"}, func() error { return nil })
	if err != nil || result.ResultState != "verified_success" || result.EffectivePricing == nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.EffectivePricing.Exact || result.EffectivePricing.Status != "verified_effective_subset" ||
		result.EffectivePricing.ChangedGroupsVerified != 1 || len(result.EffectivePricing.ChangedGroupsRawOnly) != 0 {
		t.Fatalf("effective evidence=%#v", result.EffectivePricing)
	}
}

func TestControlledPricingOptionAllowsInactiveChangedGroupWithRawReadbackEvidence(t *testing.T) {
	t.Parallel()
	site := newControlledOptionSite()
	site.ratio["inactive"] = json.Number("0.2")
	site.pricingVisible = map[string]bool{"default": true, "economy": true}
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	result, err := adapter.ApplyPricingOptionControlled(context.Background(), target, "GroupRatio",
		HashDecimalMap(map[string]string{"default": "1", "economy": "0.05", "inactive": "0.2"}),
		map[string]string{"default": "1", "economy": "0.05", "inactive": "0.3"}, func() error { return nil })
	if err != nil || result.ResultState != "verified_success" || result.EffectivePricing == nil {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(result.EffectivePricing.ChangedGroupsRawOnly) != 1 || result.EffectivePricing.ChangedGroupsRawOnly[0] != "inactive" {
		t.Fatalf("effective evidence=%#v", result.EffectivePricing)
	}
}

func TestControlledChannelRoutingPreservesVisibleConfigurationAndStatusFalseIsNotFakeSuccess(t *testing.T) {
	t.Parallel()
	site := newControlledOptionSite()
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	preview, err := adapter.PreviewChannelRouting(context.Background(), target, 5,
		domainRoutingGroup("stable"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.ApplyChannelRoutingControlled(context.Background(), target, 5, preview.ExpectedHash,
		domainRoutingGroup("stable"), func() error { return nil })
	if err != nil || result.ResultState != "verified_success" {
		t.Fatalf("routing result=%#v err=%v", result, err)
	}
	site.mu.Lock()
	requestBody := site.routingRequestBody
	site.statusDataFalse = true
	site.mu.Unlock()
	for _, field := range []string{"id", "type", "name", "models", "group", "priority", "weight", "auto_ban", "model_mapping", "remark"} {
		if _, exists := requestBody[field]; !exists {
			t.Fatalf("routing request omitted preserved field %q: %#v", field, requestBody)
		}
	}
	statusPreview, err := adapter.PreviewChannelStatus(context.Background(), target, 5, channelStatusManuallyDisabled)
	if err != nil {
		t.Fatal(err)
	}
	result, err = adapter.ApplyChannelStatusControlled(context.Background(), target, 5, statusPreview.ExpectedHash,
		channelStatusManuallyDisabled, func() error { return nil })
	if err == nil || result.ResultState != "ambiguous_after_write" {
		t.Fatalf("data:false result=%#v err=%v", result, err)
	}
}

func TestControlledMutationRequiresFreshFenceBeforeWrite(t *testing.T) {
	t.Parallel()
	site := newControlledOptionSite()
	server := httptest.NewServer(site)
	defer server.Close()
	adapter, target := newTargetAdapter(t, server)
	result, err := adapter.ApplyPricingOptionControlled(context.Background(), target, "GroupRatio",
		HashDecimalMap(map[string]string{"default": "1", "economy": "0.05"}),
		map[string]string{"default": "1", "economy": "0.08"}, func() error { return errors.New("stale fencing token") })
	if err == nil || result.ResultState != "rejected_before_write" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	site.mu.Lock()
	defer site.mu.Unlock()
	if site.putCount != 0 {
		t.Fatalf("stale fence performed %d writes", site.putCount)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func domainRoutingGroup(value string) domain.TargetChannelRoutingUpdate {
	return domain.TargetChannelRoutingUpdate{Group: &value}
}
