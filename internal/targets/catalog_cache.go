package targets

import (
	"fmt"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

type targetCatalogCache = sharedReadCache[domain.CatalogSnapshot]

func newTargetCatalogCache() *targetCatalogCache {
	return newSharedReadCache[domain.CatalogSnapshot](targetStructuralReadTTL, targetReadFailureTTL)
}

func targetCatalogCacheKey(target domain.TargetSite) string {
	return fmt.Sprintf("%d|%s|%d", target.ID,
		strings.ToLower(strings.TrimRight(strings.TrimSpace(target.BaseURL), "/")), target.CredentialID)
}
