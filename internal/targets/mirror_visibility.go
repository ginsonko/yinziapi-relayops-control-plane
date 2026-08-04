package targets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ginsonko/yinziapi-relayops-control-plane/internal/domain"
)

const mirrorVisibilitySchema = "newapi-mirror-visibility-v1"

type MirrorVisibilityAdapter interface {
	ReadMirrorVisibility(context.Context, domain.TargetSite, int64, string) (MirrorVisibilityReceipt, error)
}

type MirrorVisibilityReceipt struct {
	Schema             string `json:"schema"`
	ChannelID          int64  `json:"channel_id"`
	Group              string `json:"group"`
	ChannelStateHash   string `json:"channel_state_hash"`
	ModelSetHash       string `json:"model_set_hash"`
	ModelCount         int    `json:"model_count"`
	ContractRevision   string `json:"contract_revision"`
	MarketplaceHash    string `json:"marketplace_hash"`
	AbilityHash        string `json:"ability_hash"`
	CacheHash          string `json:"cache_hash"`
	MarketplaceVisible bool   `json:"marketplace_visible"`
	AbilityVisible     bool   `json:"ability_visible"`
	CacheVisible       bool   `json:"cache_visible"`
	CacheMode          string `json:"cache_mode"`
	EvidenceHash       string `json:"evidence_hash"`
}

func AsMirrorVisibilityAdapter(adapter Adapter) (MirrorVisibilityAdapter, error) {
	value, ok := adapter.(MirrorVisibilityAdapter)
	if !ok {
		return nil, fmt.Errorf("target adapter %q does not support mirror visibility receipts", adapter.Name())
	}
	return value, nil
}

func (a *NewAPI) ReadMirrorVisibility(ctx context.Context, target domain.TargetSite, channelID int64,
	group string) (MirrorVisibilityReceipt, error) {
	group = strings.TrimSpace(group)
	if channelID <= 0 || group == "" || len(group) > 64 || strings.ContainsAny(group, ",\x00\r\n") {
		return MirrorVisibilityReceipt{}, errors.New("mirror visibility channel and group are invalid")
	}
	body, err := a.managementRequest(ctx, target, http.MethodGet,
		"/api/channel/"+strconv.FormatInt(channelID, 10)+"/mirror_visibility",
		url.Values{"group": []string{group}}, nil)
	if err != nil {
		return MirrorVisibilityReceipt{}, fmt.Errorf("read mirror visibility: %w", err)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return MirrorVisibilityReceipt{}, fmt.Errorf("decode mirror visibility envelope: %w", err)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return MirrorVisibilityReceipt{}, errors.New("mirror visibility response omitted data")
	}
	var receipt MirrorVisibilityReceipt
	decoder := json.NewDecoder(bytes.NewReader(envelope.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return MirrorVisibilityReceipt{}, fmt.Errorf("decode mirror visibility receipt: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return MirrorVisibilityReceipt{}, err
	}
	if err := validateMirrorVisibilityReceipt(receipt, channelID, group); err != nil {
		return MirrorVisibilityReceipt{}, err
	}
	return receipt, nil
}

func validateMirrorVisibilityReceipt(receipt MirrorVisibilityReceipt, channelID int64, group string) error {
	if receipt.Schema != mirrorVisibilitySchema {
		return fmt.Errorf("unsupported mirror visibility schema %q", receipt.Schema)
	}
	if receipt.ChannelID != channelID || receipt.Group != group {
		return errors.New("mirror visibility receipt binding differs from the request")
	}
	if receipt.ModelCount < 0 || receipt.ModelCount > 50_000 {
		return errors.New("mirror visibility model count is outside the supported range")
	}
	for label, value := range map[string]string{
		"channel_state_hash": receipt.ChannelStateHash, "model_set_hash": receipt.ModelSetHash,
		"contract_revision": receipt.ContractRevision, "marketplace_hash": receipt.MarketplaceHash,
		"ability_hash": receipt.AbilityHash, "cache_hash": receipt.CacheHash, "evidence_hash": receipt.EvidenceHash,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("mirror visibility %s is not a canonical SHA-256", label)
		}
	}
	if receipt.CacheMode != "memory" && receipt.CacheMode != "database_direct" {
		return fmt.Errorf("unsupported mirror visibility cache mode %q", receipt.CacheMode)
	}
	if HashMirrorVisibilityReceipt(receipt) != receipt.EvidenceHash {
		return errors.New("mirror visibility evidence hash does not match its receipt")
	}
	return nil
}

func HashMirrorModelSet(group string, models []string) string {
	normalized := normalizeModels(models)
	hash := sha256.New()
	writeMirrorHashPart(hash, "mirror-model-set/v1")
	writeMirrorHashPart(hash, strings.TrimSpace(group))
	for _, modelName := range normalized {
		writeMirrorHashPart(hash, modelName)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func HashMirrorVisibilityReceipt(receipt MirrorVisibilityReceipt) string {
	hash := sha256.New()
	for _, value := range []string{
		receipt.Schema, strconv.FormatInt(receipt.ChannelID, 10), receipt.Group,
		receipt.ChannelStateHash, receipt.ModelSetHash, strconv.Itoa(receipt.ModelCount),
		receipt.ContractRevision, receipt.MarketplaceHash, receipt.AbilityHash, receipt.CacheHash,
		strconv.FormatBool(receipt.MarketplaceVisible), strconv.FormatBool(receipt.AbilityVisible),
		strconv.FormatBool(receipt.CacheVisible), receipt.CacheMode,
	} {
		writeMirrorHashPart(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeMirrorHashPart(hash io.Writer, value string) {
	_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
	_, _ = hash.Write([]byte{':'})
	_, _ = hash.Write([]byte(value))
	_, _ = hash.Write([]byte{0})
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing mirror visibility data: %w", err)
	}
	return errors.New("mirror visibility receipt contains trailing JSON")
}
