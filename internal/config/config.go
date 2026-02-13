// Package config provides configuration types, parsing, validation,
// and hot-reload watching for the kuberture controller.
package config

import (
	"os"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"gopkg.in/yaml.v3"
)

const (
	// AddressSourceEndpointSlice uses EndpointSlice resources to discover addresses.
	AddressSourceEndpointSlice = "endpointslice"
	// AddressSourceNodeInternal uses node internal IPs.
	AddressSourceNodeInternal = "node-internal"
	// AddressSourceNodeExternal uses node external IPs.
	AddressSourceNodeExternal = "node-external"
	// AddressSourceNodePublic uses node public IPs.
	AddressSourceNodePublic = "node-public"

	// AddressTypeIPv4 selects IPv4 addresses.
	AddressTypeIPv4 = "IPv4"
	// AddressTypeIPv6 selects IPv6 addresses.
	AddressTypeIPv6 = "IPv6"

	defaultNamespace        = "default"
	defaultServiceName      = "kubernetes"
	defaultMetricsBindAddr  = ":8080"
	defaultHealthProbeAddr  = ":8081"
	defaultAnnotationPrefix = "external-dns.alpha.kubernetes.io/"
	defaultRecordTTL        = 60
	defaultAddressSource    = AddressSourceEndpointSlice
	defaultAddressType      = AddressTypeIPv4
	debounceDuration        = 100 * time.Millisecond
)

// isValidAddressSource reports whether the given source is a valid address source.
func isValidAddressSource(source string) bool {
	switch source {
	case AddressSourceEndpointSlice,
		AddressSourceNodeInternal,
		AddressSourceNodeExternal,
		AddressSourceNodePublic:
		return true
	default:
		return false
	}
}

// isValidAddressType reports whether the given typ is a valid address type.
func isValidAddressType(typ string) bool {
	switch typ {
	case AddressTypeIPv4, AddressTypeIPv6:
		return true
	default:
		return false
	}
}

// isValidDNSLabel reports whether name is a valid RFC 1123 DNS label:
// 1-63 characters, lowercase alphanumeric, hyphens allowed (not at start or end).
func isValidDNSLabel(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}

	for i, ch := range name {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' {
			continue
		}

		if ch == '-' && i > 0 && i < len(name)-1 {
			continue
		}

		return false
	}

	return true
}

// isHostnameChar reports whether ch is valid in a DNS label (alphanumeric or hyphen).
func isHostnameChar(ch rune) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') || ch == '-'
}

// isValidLabel checks a single DNS label for RFC 1123 compliance.
func isValidLabel(label string) bool {
	const maxLabelLen = 63

	if label == "" || len(label) > maxLabelLen {
		return false
	}

	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}

	for _, ch := range label {
		if !isHostnameChar(ch) {
			return false
		}
	}

	return true
}

// isValidHostname checks whether h is a valid DNS hostname per RFC 952/1123.
// It accepts optional wildcard prefix (*.example.com) and trailing dot.
// Requires at least two labels (e.g. "example.com", not "localhost").
func isValidHostname(h string) bool {
	if h == "" {
		return false
	}

	check := strings.TrimSuffix(h, ".")

	const maxHostnameLen = 253
	if len(check) > maxHostnameLen {
		return false
	}

	if strings.HasPrefix(check, "*.") {
		check = check[2:]
	} else if strings.Contains(check, "*") {
		return false
	}

	labels := strings.Split(check, ".")
	if len(labels) < 2 {
		return false
	}

	for _, label := range labels {
		if !isValidLabel(label) {
			return false
		}
	}

	return true
}

// Config is the top-level configuration for the kuberture controller.
type Config struct {
	Source                 SourceConfig   `yaml:"source"`
	Outputs                []OutputConfig `yaml:"outputs"`
	MetricsBindAddress     string         `yaml:"metricsBindAddress"`
	HealthProbeBindAddress string         `yaml:"healthProbeBindAddress"`
}

// SourceConfig defines the source Kubernetes service to watch.
type SourceConfig struct {
	Namespace   string `yaml:"namespace"`
	ServiceName string `yaml:"serviceName"`
}

// OutputConfig defines a single DNS output target.
type OutputConfig struct {
	Name             string   `yaml:"name"`
	Hostnames        []string `yaml:"hostname"` //nolint:tagliatelle // user-facing YAML field name is intentionally singular
	AnnotationPrefix string   `yaml:"annotationPrefix"`
	ServiceName      string   `yaml:"serviceName"`
	RecordTTL        int      `yaml:"recordTTL"` //nolint:tagliatelle // TTL is a well-known abbreviation, keep uppercase
	AddressSource    string   `yaml:"addressSource"`
	AddressType      string   `yaml:"addressType"`
}

// Load reads a YAML config file, applies defaults, and validates it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, "reading config file")
	}

	cfg := &Config{}

	unmarshalErr := yaml.Unmarshal(data, cfg)
	if unmarshalErr != nil {
		return nil, errors.Wrap(unmarshalErr, "parsing config YAML")
	}

	applyDefaults(cfg)

	validationErr := validate(cfg)
	if validationErr != nil {
		return nil, errors.Wrap(validationErr, "validating config")
	}

	return cfg, nil
}

func applyDefaults(cfg *Config) {
	applySourceDefaults(cfg)
	applyBindDefaults(cfg)
	applyOutputDefaults(cfg)
}

func applySourceDefaults(cfg *Config) {
	if cfg.Source.Namespace == "" {
		cfg.Source.Namespace = defaultNamespace
	}

	if cfg.Source.ServiceName == "" {
		cfg.Source.ServiceName = defaultServiceName
	}
}

func applyBindDefaults(cfg *Config) {
	if cfg.MetricsBindAddress == "" {
		cfg.MetricsBindAddress = defaultMetricsBindAddr
	}

	if cfg.HealthProbeBindAddress == "" {
		cfg.HealthProbeBindAddress = defaultHealthProbeAddr
	}
}

func applyOutputDefaults(cfg *Config) {
	for idx := range cfg.Outputs {
		out := &cfg.Outputs[idx]

		if out.AnnotationPrefix == "" {
			out.AnnotationPrefix = defaultAnnotationPrefix
		}

		if out.RecordTTL == 0 {
			out.RecordTTL = defaultRecordTTL
		}

		if out.AddressSource == "" {
			out.AddressSource = defaultAddressSource
		}

		if out.AddressType == "" {
			out.AddressType = defaultAddressType
		}
	}
}

func validate(cfg *Config) error {
	if len(cfg.Outputs) == 0 {
		return errors.New("at least one output is required")
	}

	fieldsErr := validateOutputFields(cfg.Outputs)
	if fieldsErr != nil {
		return fieldsErr
	}

	return validateOutputUniqueness(cfg.Outputs)
}

func validateOutputFields(outputs []OutputConfig) error {
	for idx := range outputs {
		fieldErr := validateSingleOutput(&outputs[idx])
		if fieldErr != nil {
			return fieldErr
		}
	}

	return nil
}

func validateSingleOutput(out *OutputConfig) error {
	if out.Name == "" {
		return errors.New("output name is required")
	}

	if len(out.Hostnames) == 0 {
		return errors.Wrap(
			errors.New("at least one hostname is required"),
			"output "+out.Name,
		)
	}

	for _, h := range out.Hostnames {
		if !isValidHostname(h) {
			return errors.Wrapf(
				errors.Newf("invalid hostname %q", h),
				"output %s", out.Name,
			)
		}
	}

	if out.ServiceName == "" {
		return errors.Wrap(
			errors.New("serviceName is required"),
			"output "+out.Name,
		)
	}

	if !isValidDNSLabel(out.ServiceName) {
		return errors.Wrapf(
			errors.Newf("serviceName %q is not a valid DNS label", out.ServiceName),
			"output %s", out.Name,
		)
	}

	return validateOutputEnums(out)
}

func validateOutputEnums(out *OutputConfig) error {
	if !isValidAddressSource(out.AddressSource) {
		return errors.Wrapf(
			errors.Newf("invalid addressSource %q", out.AddressSource),
			"output %s", out.Name,
		)
	}

	if !isValidAddressType(out.AddressType) {
		return errors.Wrapf(
			errors.Newf("invalid addressType %q", out.AddressType),
			"output %s", out.Name,
		)
	}

	if !strings.HasSuffix(out.AnnotationPrefix, "/") {
		return errors.Wrapf(
			errors.New("annotationPrefix must end with /"),
			"output %s", out.Name,
		)
	}

	const (
		minRecordTTL = 1
		maxRecordTTL = 86400
	)

	if out.RecordTTL < minRecordTTL || out.RecordTTL > maxRecordTTL {
		return errors.Wrapf(
			errors.Newf("recordTTL must be between %d and %d, got %d", minRecordTTL, maxRecordTTL, out.RecordTTL),
			"output %s", out.Name,
		)
	}

	return nil
}

func validateOutputUniqueness(outputs []OutputConfig) error {
	namesErr := validateUniqueNames(outputs)
	if namesErr != nil {
		return namesErr
	}

	svcErr := validateUniqueServiceNames(outputs)
	if svcErr != nil {
		return svcErr
	}

	return validateUniqueHostnames(outputs)
}

func validateUniqueNames(outputs []OutputConfig) error {
	seen := make(map[string]bool, len(outputs))

	for idx := range outputs {
		name := outputs[idx].Name
		if seen[name] {
			return errors.Wrap(
				errors.New("duplicate output name"),
				name,
			)
		}

		seen[name] = true
	}

	return nil
}

func validateUniqueServiceNames(outputs []OutputConfig) error {
	seen := make(map[string]bool, len(outputs))

	for idx := range outputs {
		name := outputs[idx].ServiceName
		if seen[name] {
			return errors.Wrap(
				errors.New("duplicate serviceName"),
				name,
			)
		}

		seen[name] = true
	}

	return nil
}

func validateUniqueHostnames(outputs []OutputConfig) error {
	seen := make(map[string]string) // hostname → output name

	for idx := range outputs {
		for _, hostname := range outputs[idx].Hostnames {
			if prevOutput, exists := seen[hostname]; exists {
				return errors.Wrapf(
					errors.Newf("hostname %q already used by output %q", hostname, prevOutput),
					"output %s", outputs[idx].Name,
				)
			}

			seen[hostname] = outputs[idx].Name
		}
	}

	return nil
}
