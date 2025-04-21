package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var entryLog = log.Log.WithName("config")

// Config holds the application configuration values
// Note: Viper uses mapstructure tags for config file/env var mapping.
// We define mapstructure tags corresponding to the keys in a potential config file (e.g., yaml/json/toml).
// Viper's AutomaticEnv will convert env vars like POD_HOUSEKEEPER_MARK_DURATION to markDuration automatically.
type Config struct {
	MarkDuration            time.Duration       `mapstructure:"markDuration"`
	DeleteDuration          time.Duration       `mapstructure:"deleteDuration"`
	MaxMarkedPerOwner       int                 `mapstructure:"maxMarkedPerOwner"`       // Default limit if Kind not in MaxMarkedPerOwnerByKind
	MaxMarkedPerOwnerByKind map[string]int      `mapstructure:"maxMarkedPerOwnerByKind"` // Kind-specific limits (e.g., "ReplicaSet": 2)
	MaxConcurrentReconciles int                 `mapstructure:"maxConcurrentReconciles"`
	ExcludeSelf             bool                `mapstructure:"excludeSelf"`
	CheckExcludeAnnotation  bool                `mapstructure:"checkExcludeAnnotation"` // Check for exclude annotation on Pods
	ExcludedNamespacesStr   string              `mapstructure:"excludedNamespaces"`     // Raw comma-separated string from config/env
	ExcludedNamespaces      map[string]struct{} `mapstructure:"-"`                      // Parsed map, explicitly ignore during viper unmarshal
	// MetricsBindAddress    string // Removed - Use default :8080
	// HealthProbeBindAddress  string // Removed - Use default :8081
}

// LoadConfig loads configuration from file (if provided), environment variables, and defaults using Viper.
// It uses podInfoPresent to determine the default for ExcludeSelf if not explicitly set.
func LoadConfig(configFile string, podInfoPresent bool) (Config, error) {
	v := viper.New()

	// --- Set Defaults (excluding excludeSelf) ---
	v.SetDefault("markDuration", "24h")
	v.SetDefault("deleteDuration", "48h")
	v.SetDefault("excludedNamespaces", "")
	v.SetDefault("maxMarkedPerOwner", 1)
	v.SetDefault("maxConcurrentReconciles", 2)
	v.SetDefault("checkExcludeAnnotation", false) // Default to not checking the annotation
	// v.SetDefault("excludeSelf", false) // Default is now conditional

	// --- Load from Config File (if provided) ---
	if configFile != "" {
		v.SetConfigFile(configFile)
		if err := v.ReadInConfig(); err != nil {
			// Handle specific error types if needed (e.g., viper.ConfigFileNotFoundError)
			// For now, just log and potentially return the error if critical
			if _, ok := err.(viper.ConfigFileNotFoundError); ok {
				entryLog.Info("Config file specified but not found, proceeding with env vars and defaults", "file", configFile)
			} else {
				entryLog.Error(err, "Failed to read config file", "file", configFile)
				return Config{}, fmt.Errorf("failed to read config file %s: %w", configFile, err)
			}
		} else {
			entryLog.Info("Successfully loaded configuration from file", "file", configFile)
		}
	}

	// --- Load from Environment Variables ---
	v.SetEnvPrefix("POD_HOUSEKEEPER")
	// Bind environment variables explicitly
	var err error                                                                     // Declare err once before the block
	if err = v.BindEnv("markDuration", "POD_HOUSEKEEPER_MARK_DURATION"); err != nil { // Use = instead of := for subsequent calls
		entryLog.Error(err, "Failed to bind env var POD_HOUSEKEEPER_MARK_DURATION")
	}
	if err = v.BindEnv("deleteDuration", "POD_HOUSEKEEPER_DELETE_DURATION"); err != nil {
		entryLog.Error(err, "Failed to bind env var POD_HOUSEKEEPER_DELETE_DURATION")
	}
	if err = v.BindEnv("excludedNamespaces", "POD_HOUSEKEEPER_EXCLUDED_NAMESPACES"); err != nil {
		entryLog.Error(err, "Failed to bind env var POD_HOUSEKEEPER_EXCLUDED_NAMESPACES")
	}
	if err = v.BindEnv("maxMarkedPerOwner", "POD_HOUSEKEEPER_MAX_MARKED_PER_OWNER"); err != nil {
		entryLog.Error(err, "Failed to bind env var POD_HOUSEKEEPER_MAX_MARKED_PER_OWNER")
	}
	if err = v.BindEnv("maxConcurrentReconciles", "POD_HOUSEKEEPER_MAX_CONCURRENT_RECONCILES"); err != nil {
		entryLog.Error(err, "Failed to bind env var POD_HOUSEKEEPER_MAX_CONCURRENT_RECONCILES")
	}
	if err = v.BindEnv("excludeSelf", "POD_HOUSEKEEPER_EXCLUDE_SELF"); err != nil {
		entryLog.Error(err, "Failed to bind env var POD_HOUSEKEEPER_EXCLUDE_SELF")
	}
	if err = v.BindEnv("checkExcludeAnnotation", "POD_HOUSEKEEPER_CHECK_EXCLUDE_ANNOTATION"); err != nil {
		entryLog.Error(err, "Failed to bind env var POD_HOUSEKEEPER_CHECK_EXCLUDE_ANNOTATION")
	}

	// --- Unmarshal into Config struct with Standard Decode Hook ---
	var cfg Config
	if err = v.Unmarshal(&cfg, viper.DecodeHook(mapstructure.StringToTimeDurationHookFunc())); err != nil {
		entryLog.Error(err, "Failed to unmarshal configuration")
		return cfg, fmt.Errorf("failed to unmarshal configuration: %w", err)
	}

	// --- Set Conditional Default for ExcludeSelf ---
	// Check if the user explicitly set the value via env var or config file.
	if !v.IsSet("excludeSelf") {
		// If not set by user, apply default based on pod info presence.
		cfg.ExcludeSelf = podInfoPresent
		entryLog.V(1).Info("ExcludeSelf not explicitly set, applying default based on pod info", "podInfoPresent", podInfoPresent, "appliedDefault", cfg.ExcludeSelf)
	} else {
		entryLog.V(1).Info("ExcludeSelf explicitly set by user", "value", cfg.ExcludeSelf)
	}

	// --- Post-Unmarshal Validation/Adjustments ---

	// Handle MaxConcurrentReconciles: Use flag value if set (>0), otherwise stick with value from env/config/default.
	// The default from viper/env/config is already in cfg.MaxConcurrentReconciles.
	// If the flag was set via pflag (and bound to viper), viper.GetInt("max-concurrent-reconciles") will return it.
	flagVal := v.GetInt("max-concurrent-reconciles") // Check the specific key bound from pflag
	if flagVal > 0 {                                 // If flag was explicitly set to a positive value
		cfg.MaxConcurrentReconciles = flagVal
		entryLog.V(1).Info("Using MaxConcurrentReconciles from command-line flag", "value", cfg.MaxConcurrentReconciles)
	} else {
		entryLog.V(1).Info("Command-line flag --max-concurrent-reconciles not set or zero, using value from env/config/default", "value", cfg.MaxConcurrentReconciles)
	}

	// Ensure integers are >= 1 (Apply after potentially overriding with flag)
	if cfg.MaxMarkedPerOwner < 1 {
		entryLog.Info("maxMarkedPerOwner (default) must be >= 1, setting to default 1", "originalValue", cfg.MaxMarkedPerOwner)
		cfg.MaxMarkedPerOwner = 1
	}
	if cfg.MaxConcurrentReconciles < 1 {
		// This handles cases where flag is 0 or env/config gave < 1
		entryLog.Info("maxConcurrentReconciles must be >= 1, setting to default 2", "finalValueFromFlag/Env/Config", cfg.MaxConcurrentReconciles)
		cfg.MaxConcurrentReconciles = 2 // Default defined earlier, but enforce minimum here
	}

	// Ensure kind-specific limits are >= 1
	if cfg.MaxMarkedPerOwnerByKind == nil {
		cfg.MaxMarkedPerOwnerByKind = make(map[string]int) // Initialize if nil
	} else {
		for kind, limit := range cfg.MaxMarkedPerOwnerByKind {
			if limit < 1 {
				entryLog.Info("maxMarkedPerOwnerByKind value must be >= 1, setting to default 1", "kind", kind, "originalValue", limit)
				cfg.MaxMarkedPerOwnerByKind[kind] = 1
			}
		}
	}

	// Parse Excluded Namespaces string into map
	cfg.ExcludedNamespaces = make(map[string]struct{})
	if cfg.ExcludedNamespacesStr != "" {
		for ns := range strings.SplitSeq(cfg.ExcludedNamespacesStr, ",") {
			trimmedNs := strings.TrimSpace(ns)
			if trimmedNs != "" {
				cfg.ExcludedNamespaces[trimmedNs] = struct{}{}
			}
		}
	}

	// --- Sanity Checks --- (Same as before)
	if cfg.DeleteDuration <= cfg.MarkDuration {
		err := fmt.Errorf("deleteDuration (%s) must be strictly greater than markDuration (%s)", cfg.DeleteDuration, cfg.MarkDuration) // Declare err here if not declared above
		entryLog.Error(err, "Configuration validation failed")
		return cfg, err
	}

	// --- Log Final Configuration ---
	logFinalConfig(cfg) // Use a simplified logging function now

	return cfg, nil
}

// getEnvWithDefault - REMOVED (Handled by Viper)

// parseIntEnv - REMOVED (Partially handled by Viper, validation added in LoadConfig)

// logConfig - Renamed and Simplified to logFinalConfig
func logFinalConfig(cfg Config) {
	excludedList := []string{}
	for ns := range cfg.ExcludedNamespaces {
		excludedList = append(excludedList, ns)
	}

	// Log key final values
	entryLog.Info("Using final configuration",
		"markDuration", cfg.MarkDuration.String(),
		"deleteDuration", cfg.DeleteDuration.String(),
		"excludedNamespacesRaw", cfg.ExcludedNamespacesStr,
		"maxMarkedPerOwnerDefault", cfg.MaxMarkedPerOwner,
		"maxMarkedPerOwnerByKind", cfg.MaxMarkedPerOwnerByKind, // Log the map
		"maxConcurrentReconciles", cfg.MaxConcurrentReconciles,
		"excludeSelf", cfg.ExcludeSelf,
		"checkExcludeAnnotation", cfg.CheckExcludeAnnotation, // Log the new flag
		// Add other important derived values if needed
	)
	entryLog.V(1).Info("Parsed excluded namespaces map", "keys", excludedList)
}

// IsExcluded checks if a namespace is in the exclusion list.
func (c *Config) IsExcluded(namespace string) bool {
	_, excluded := c.ExcludedNamespaces[namespace]
	return excluded
}
