package httpapi

import (
	"net/http"
	"testing"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

func TestProtocol2SupplierDeletionRetentionErrorIsConflict(t *testing.T) {
	if got := statusCode(store.ErrSupplierDeletionAuthorityRetentionRequired); got != http.StatusConflict {
		t.Fatalf("statusCode=%d want=%d", got, http.StatusConflict)
	}
}
