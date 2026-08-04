package domain

import "time"

const ConnectorManifestSchemaV1 = "relayops.connector-manifest/v1"

// ConnectorManifest is a versioned, portable description of one native
// upstream contract. It contains endpoint and header semantics only. Secrets,
// executable expressions, arbitrary templates and cross-origin URLs are not
// part of the format.
type ConnectorManifest struct {
	SchemaVersion string                                 `json:"schema_version"`
	Name          string                                 `json:"name"`
	AdapterFamily string                                 `json:"adapter_family"`
	Origins       ConnectorManifestOrigins               `json:"origins"`
	Auth          ConnectorManifestAuth                  `json:"auth,omitempty"`
	Capabilities  map[string]ConnectorManifestCapability `json:"capabilities"`
	Login         *CredentialLoginRuleView               `json:"login,omitempty"`
}

type ConnectorManifestOrigins struct {
	Schemes []string `json:"schemes"`
	Hosts   []string `json:"hosts"`
}

type ConnectorManifestAuth struct {
	Header     string `json:"header,omitempty"`
	Scheme     string `json:"scheme,omitempty"`
	UserHeader string `json:"user_header,omitempty"`
}

type ConnectorManifestCapability struct {
	Paths     []string               `json:"paths"`
	Contract  string                 `json:"contract,omitempty"`
	Auth      *ConnectorManifestAuth `json:"auth,omitempty"`
	JSONPaths map[string]string      `json:"json_paths,omitempty"`
}

type ConnectorManifestRecord struct {
	ID                int64             `json:"id"`
	Name              string            `json:"name"`
	SchemaVersion     string            `json:"schema_version"`
	AdapterFamily     string            `json:"adapter_family"`
	Manifest          ConnectorManifest `json:"manifest"`
	Source            string            `json:"source"`
	ValidationStatus  string            `json:"validation_status"`
	ValidationMessage string            `json:"validation_message,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type SupplierManifestBinding struct {
	SupplierID  int64                    `json:"supplier_id"`
	ManifestID  int64                    `json:"manifest_id"`
	BoundOrigin string                   `json:"bound_origin"`
	BoundAt     time.Time                `json:"bound_at"`
	Manifest    *ConnectorManifestRecord `json:"manifest,omitempty"`
}
