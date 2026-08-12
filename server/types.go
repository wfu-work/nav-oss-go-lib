// Package server provides the optional nav-oss management API used by the
// embedded web console. Importing the root oss package remains free of HTTP
// server and console concerns.
package server

import "time"

// Config configures the nav-oss management API.
type Config struct {
	StateFile         string
	RuntimeDir        string
	AdminUsername     string
	AdminPassword     string
	AdminPasswordHash string
	SessionTTL        time.Duration
	APIAddress        string
	ConsoleAddress    string
}

// ProviderInput is the editable provider configuration accepted by the API.
type ProviderInput struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	RootDir          string `json:"rootDir"`
	Endpoint         string `json:"endpoint"`
	Region           string `json:"region"`
	Bucket           string `json:"bucket"`
	Prefix           string `json:"prefix"`
	CredentialMode   string `json:"credentialMode"`
	BucketLookup     string `json:"bucketLookup"`
	AccessKeyID      string `json:"accessKeyId"`
	SecretAccessKey  string `json:"secretAccessKey"`
	SessionToken     string `json:"sessionToken"`
	AutoCreateBucket bool   `json:"autoCreateBucket"`
	SkipBucketCheck  bool   `json:"skipBucketCheck"`
	Enabled          bool   `json:"enabled"`
	IsDefault        bool   `json:"isDefault"`
}

// Provider is the secret-free provider representation returned to clients.
type Provider struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Type           string     `json:"type"`
	Endpoint       string     `json:"endpoint,omitempty"`
	Region         string     `json:"region,omitempty"`
	Bucket         string     `json:"bucket,omitempty"`
	Prefix         string     `json:"prefix,omitempty"`
	RootDir        string     `json:"rootDir,omitempty"`
	CredentialMode string     `json:"credentialMode,omitempty"`
	BucketLookup   string     `json:"bucketLookup,omitempty"`
	Enabled        bool       `json:"enabled"`
	Health         string     `json:"health"`
	IsDefault      bool       `json:"isDefault"`
	ObjectCount    int64      `json:"objectCount"`
	UsedBytes      int64      `json:"usedBytes"`
	LastCheckedAt  *time.Time `json:"lastCheckedAt,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

type providerState struct {
	ProviderInput
	ID            string     `json:"id"`
	Health        string     `json:"health"`
	ObjectCount   int64      `json:"objectCount"`
	UsedBytes     int64      `json:"usedBytes"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// Settings controls console defaults that are independent from a provider.
type Settings struct {
	DefaultProviderID         string `json:"defaultProviderId"`
	PresignGetExpiryMinutes   int    `json:"presignGetExpiryMinutes"`
	PresignPutExpiryMinutes   int    `json:"presignPutExpiryMinutes"`
	ListPageSize              int    `json:"listPageSize"`
	MaxListPageSize           int    `json:"maxListPageSize"`
	RequireDeleteConfirmation bool   `json:"requireDeleteConfirmation"`
	AllowAnonymousPresign     bool   `json:"allowAnonymousPresign"`
}

// Operation records a management action initiated through the console API.
type Operation struct {
	ID           string    `json:"id"`
	Action       string    `json:"action"`
	Resource     string    `json:"resource"`
	ProviderName string    `json:"providerName"`
	Operator     string    `json:"operator"`
	DurationMS   int64     `json:"durationMs"`
	Result       string    `json:"result"`
	ErrorSummary string    `json:"errorSummary,omitempty"`
	OccurredAt   time.Time `json:"occurredAt"`
}

// SystemInfo describes the running management service without exposing secrets.
type SystemInfo struct {
	Service              string    `json:"service"`
	Version              string    `json:"version"`
	GoVersion            string    `json:"goVersion"`
	APIAddress           string    `json:"apiAddress"`
	ConsoleAddress       string    `json:"consoleAddress"`
	RuntimeDir           string    `json:"runtimeDir"`
	StartedAt            time.Time `json:"startedAt"`
	UptimeSeconds        int64     `json:"uptimeSeconds"`
	SessionTTLMinutes    int64     `json:"sessionTTLMinutes"`
	ProviderCount        int       `json:"providerCount"`
	EnabledProviderCount int       `json:"enabledProviderCount"`
	HealthyProviderCount int       `json:"healthyProviderCount"`
	CurrentSessionCount  int       `json:"currentSessionCount"`
}

// RuntimeCatalogItem describes an object-storage binary that the management
// service knows how to install without accepting arbitrary executable input.
type RuntimeCatalogItem struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	License            string   `json:"license"`
	Supported          bool     `json:"supported"`
	UnsupportedReason  string   `json:"unsupportedReason,omitempty"`
	DefaultAPIPort     int      `json:"defaultApiPort"`
	DefaultConsolePort int      `json:"defaultConsolePort"`
	Verification       string   `json:"verification"`
	Platforms          []string `json:"platforms"`
}

// RuntimeInstallInput is intentionally restricted to known-safe fields. The
// API never accepts arbitrary download URLs, executable paths or arguments.
type RuntimeInstallInput struct {
	Software    string `json:"software"`
	Name        string `json:"name"`
	APIPort     int    `json:"apiPort"`
	ConsolePort int    `json:"consolePort"`
	Bucket      string `json:"bucket"`
	Region      string `json:"region"`
}

// RuntimeInstallation is the secret-free lifecycle state returned to clients.
type RuntimeInstallation struct {
	ID                  string    `json:"id"`
	Software            string    `json:"software"`
	Name                string    `json:"name"`
	Version             string    `json:"version,omitempty"`
	Status              string    `json:"status"`
	Step                string    `json:"step"`
	Progress            int       `json:"progress"`
	APIPort             int       `json:"apiPort"`
	ConsolePort         int       `json:"consolePort"`
	Endpoint            string    `json:"endpoint"`
	ConsoleURL          string    `json:"consoleUrl"`
	Bucket              string    `json:"bucket"`
	Region              string    `json:"region"`
	DataDir             string    `json:"dataDir"`
	BinaryPath          string    `json:"binaryPath,omitempty"`
	DownloadURL         string    `json:"downloadUrl,omitempty"`
	DownloadedBytes     int64     `json:"downloadedBytes,omitempty"`
	DownloadTotalBytes  int64     `json:"downloadTotalBytes,omitempty"`
	DownloadSpeedBytes  int64     `json:"downloadSpeedBytes,omitempty"`
	DownloadPercent     int       `json:"downloadPercent,omitempty"`
	DownloadAttempt     int       `json:"downloadAttempt,omitempty"`
	DownloadMaxAttempts int       `json:"downloadMaxAttempts,omitempty"`
	DownloadResumed     bool      `json:"downloadResumed,omitempty"`
	Checksum            string    `json:"checksum,omitempty"`
	Verification        string    `json:"verification,omitempty"`
	PID                 int       `json:"pid,omitempty"`
	ProviderID          string    `json:"providerId,omitempty"`
	ErrorSummary        string    `json:"errorSummary,omitempty"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type persistedState struct {
	AdminUsername     string                `json:"adminUsername"`
	AdminPasswordHash string                `json:"adminPasswordHash"`
	Providers         []providerState       `json:"providers"`
	Settings          Settings              `json:"settings"`
	Operations        []Operation           `json:"operations,omitempty"`
	Installations     []RuntimeInstallation `json:"installations,omitempty"`
}

type pageResult[T any] struct {
	Data       []T    `json:"data"`
	Total      int    `json:"total"`
	Page       int    `json:"page"`
	Size       int    `json:"size"`
	NextCursor string `json:"nextCursor,omitempty"`
	HasMore    bool   `json:"hasMore,omitempty"`
}
