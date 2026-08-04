package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/store"
)

type File struct {
	Suppliers []domain.Supplier `json:"suppliers"`
}

func Load(ctx context.Context, st *store.Store, path string) ([]domain.Supplier, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap file: %w", err)
	}
	var config File
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, fmt.Errorf("decode bootstrap file: %w", err)
	}
	result := make([]domain.Supplier, 0, len(config.Suppliers))
	for _, candidate := range config.Suppliers {
		candidate.BaseURL = strings.TrimRight(candidate.BaseURL, "/")
		if candidate.BaseURL == "" || candidate.Name == "" {
			return nil, errors.New("bootstrap supplier requires name and base_url")
		}
		existing, findErr := st.FindSupplierByBaseURL(ctx, candidate.BaseURL)
		if findErr == nil {
			result = append(result, existing)
			continue
		}
		if !store.IsNotFound(findErr) {
			return nil, findErr
		}
		candidate.Enabled = true
		if candidate.PollIntervalSeconds <= 0 {
			candidate.PollIntervalSeconds = 300
		}
		if err := st.CreateSupplier(ctx, &candidate); err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, nil
}
